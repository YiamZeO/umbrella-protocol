package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	mrand "math/rand"
	
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	utls "github.com/refraction-networking/utls"
)

// copyBufPool holds reusable 32 KiB buffers for bidirectional TCP proxying.
var copyBufPool = sync.Pool{New: func() any { b := make([]byte, 32*1024); return &b }}

// Global session state — all SOCKS5 connections share one TLS connection.
var (
	gServerAddr         string
	gSNI                string
	gServerPubKey       []byte
	gShortId            [8]byte
	gUDPEnabled         bool
	gCloseOnRotate      bool
	gShaper             bool
	gConnectionsTimeOut time.Duration
	gSessionsNum        int

	sessMu   []sync.Mutex
	sessions []*yamux.Session
)

func main() {
	serverAddr := flag.String("server", "", "server address, e.g. vps.example.com:443 (required)")
	publicKeyB64 := flag.String("public-key", "", "server x25519 public key in base64 (required)")
	shortIdHex := flag.String("short-id", "", "Reality short ID in hex, up to 16 chars (required)")
	sni := flag.String("sni", "cloudflare.com", "SNI to use in TLS Client Hello")
	listenAddr := flag.String("listen", "0.0.0.0:1080", "local SOCKS5 listen address")
	udpEnabled := flag.Bool("udp", true, "enable SOCKS5 UDP ASSOCIATE (false = TCP-only)")
	closeOnRotate := flag.Bool("close-on-rotate", false, "close active connections when session rotates (default: let them finish naturally)")
	shaper := flag.Bool("shaper", false, "enable Shaper behavioural traffic shaping")
	decoyTraffic := flag.Bool("decoy-traffic", false, "enable decoy traffic to blur traffic pattern")
	connTimeout := flag.Int("connections-time-out", 300, "close connection if idle > timeout seconds (0 to disable)")
	sessionsNum := flag.Int("sessions-num", 5, "number of multiplexed sessions to pool (default 5)")
	flag.Parse()

	mrand.Seed(time.Now().UnixNano())

	gUDPEnabled = *udpEnabled
	gCloseOnRotate = *closeOnRotate
	gShaper = *shaper
	gConnectionsTimeOut = time.Duration(*connTimeout) * time.Second
	gSessionsNum = *sessionsNum
	if gSessionsNum < 1 {
		gSessionsNum = 1
	}
	sessMu = make([]sync.Mutex, gSessionsNum)
	sessions = make([]*yamux.Session, gSessionsNum)

	if *shaper && !*closeOnRotate {
		log.Fatal("--shaper requires --close-on-rotate: without it multiple shapers from overlapping sessions will fight over the global throttle")
	}

	if *serverAddr == "" {
		log.Fatal("--server is required")
	}
	if *publicKeyB64 == "" {
		log.Fatal("--public-key is required")
	}
	if *shortIdHex == "" {
		log.Fatal("--short-id is required")
	}

	pubKey, err := base64.StdEncoding.DecodeString(*publicKeyB64)
	if err != nil || len(pubKey) != 32 {
		log.Fatal("invalid --public-key: must be base64 of exactly 32 bytes")
	}

	shortIdBytes, err := hex.DecodeString(*shortIdHex)
	if err != nil || len(shortIdBytes) > 8 {
		log.Fatal("invalid --short-id: must be up to 16 hex chars")
	}

	gServerAddr = *serverAddr
	gSNI = *sni
	gServerPubKey = pubKey
	copy(gShortId[:], shortIdBytes)

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", *listenAddr, err)
	}
	log.Printf("Umbrella/REALITY client on %s (SOCKS5) → %s (SNI: %s)", *listenAddr, *serverAddr, *sni)

	if *decoyTraffic {
		go runDecoyTraffic(*listenAddr)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Accept error: %v", err)
			continue
		}
		go handleSocks5(conn)
	}
}

// getSession returns a multiplexed session from the pool, creating one if needed.
// All SOCKS5 connections share a pool of TLS connections via yamux streams.
// The TLS handshake uses the REALITY protocol: authentication is embedded into
// the ClientHello via ECDH + AES-GCM-encrypted SessionID, making the connection
// indistinguishable from a real TLS handshake to a legitimate site.
func getSession() (*yamux.Session, error) {
	idx := mrand.Intn(gSessionsNum)
	sessMu[idx].Lock()
	defer sessMu[idx].Unlock()

	if sessions[idx] != nil && !sessions[idx].IsClosed() {
		return sessions[idx], nil
	}

	tcpConn, err := net.DialTimeout("tcp", gServerAddr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("tcp dial: %w", err)
	}

	uConn := utls.UClient(tcpConn, &utls.Config{
		ServerName:             gSNI,
		InsecureSkipVerify:     true,
		SessionTicketsDisabled: true,
	}, utls.HelloChrome_Auto)

	// Build the ClientHello state in memory without sending it yet.
	if err := uConn.BuildHandshakeState(); err != nil {
		go tcpConn.Close()
		return nil, fmt.Errorf("build handshake state: %w", err)
	}

	// Extract ephemeral x25519 private key generated for this handshake.
	// We need it to compute ECDH with the server's static public key.
	ksKeys := uConn.HandshakeState.State13.KeyShareKeys
	var ephPriv *ecdh.PrivateKey
	switch {
	case ksKeys != nil && ksKeys.Ecdhe != nil:
		ephPriv = ksKeys.Ecdhe
	case ksKeys != nil && ksKeys.MlkemEcdhe != nil:
		// Hybrid X25519MLKEM768: the x25519 part is in MlkemEcdhe
		ephPriv = ksKeys.MlkemEcdhe
	case uConn.HandshakeState.State13.EcdheKey != nil:
		ephPriv = uConn.HandshakeState.State13.EcdheKey
	}
	if ephPriv == nil {
		go uConn.Close()
		return nil, fmt.Errorf("no x25519 key share in ClientHello")
	}

	// ECDH: shared = X25519(clientEphemeralPriv, serverStaticPub)
	serverPub, err := ecdh.X25519().NewPublicKey(gServerPubKey)
	if err != nil {
		go uConn.Close()
		return nil, fmt.Errorf("parse server public key: %w", err)
	}
	sharedSecret, err := ephPriv.ECDH(serverPub)
	if err != nil {
		go uConn.Close()
		return nil, fmt.Errorf("ecdh: %w", err)
	}

	// authKey = HKDF-SHA256(ikm=sharedSecret, salt=random[:20], info="REALITY")
	random := uConn.HandshakeState.Hello.Random
	authKey, err := hkdf.Key(sha256.New, sharedSecret, random[:20], "REALITY", 32)
	if err != nil {
		go uConn.Close()
		return nil, fmt.Errorf("hkdf: %w", err)
	}

	// Build AAD: marshal the ClientHello with sessionId = zeros.
	// The server decrypts using the same marshaled form as additional data.
	uConn.HandshakeState.Hello.SessionId = make([]byte, 32) // zero sessionId
	if err := uConn.MarshalClientHello(); err != nil {
		go uConn.Close()
		return nil, fmt.Errorf("marshal (zero sessionId): %w", err)
	}
	aad := append([]byte(nil), uConn.HandshakeState.Hello.Raw...) // copy

	// Plaintext (16 bytes): [0:2] ver=0 [2:4] zeros [4:8] unix time [8:16] shortId
	plaintext := make([]byte, 16)
	binary.BigEndian.PutUint32(plaintext[4:], uint32(time.Now().Unix()))
	copy(plaintext[8:], gShortId[:])

	// AES-256-GCM: nonce = random[20:32] (12 bytes).
	// Output: 16 bytes data + 16 bytes tag = 32 bytes → becomes the new SessionId.
	block, err := aes.NewCipher(authKey)
	if err != nil {
		go uConn.Close()
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		go uConn.Close()
		return nil, fmt.Errorf("aes gcm: %w", err)
	}
	encSessionId := aead.Seal(nil, random[20:32], plaintext, aad)

	// Set the encrypted sessionId and re-marshal.
	uConn.HandshakeState.Hello.SessionId = encSessionId
	if err := uConn.MarshalClientHello(); err != nil {
		go uConn.Close()
		return nil, fmt.Errorf("marshal (encrypted sessionId): %w", err)
	}

	// Complete TLS handshake — sends the modified ClientHello.
	if err := uConn.Handshake(); err != nil {
		go tcpConn.Close()
		return nil, fmt.Errorf("tls handshake: %w", err)
	}

	muxCfg := yamux.DefaultConfig()
	muxCfg.MaxStreamWindowSize = 4 * 1024 * 1024
	// Telegram relies on long-lived idle connections for push events.
	muxCfg.EnableKeepAlive = true
	muxCfg.KeepAliveInterval = 10 * time.Second
	muxCfg.StreamCloseTimeout = 10 * time.Second
	muxCfg.ConnectionWriteTimeout = 5 * time.Minute
	newSess, err := yamux.Client(uConn, muxCfg)
	if err != nil {
		go uConn.Close()
		return nil, fmt.Errorf("yamux client: %w", err)
	}

	sessions[idx] = newSess
	go scheduleReconnect(newSess)
	if gShaper {
		if ctrlStream, err := newSess.Open(); err == nil {
			if _, err := ctrlStream.Write([]byte{0x02}); err == nil {
				go readPhaseUpdates(ctrlStream)
			} else {
				go ctrlStream.Close()
			}
		}
	}

	log.Printf("REALITY session established to %s", gServerAddr)
	return sessions[idx], nil
}

// scheduleReconnect waits a random interval (3–15 min) then removes the session
// from global state. Active yamux streams finish naturally; the next SOCKS5
// request will transparently create a fresh TLS connection with a new handshake.
func scheduleReconnect(s *yamux.Session) {
	// Random delay in [3, 15) minutes using crypto/rand for unpredictability.
	const minDelay = 3 * time.Minute
	const maxJitter = 12 * time.Minute
	n, err := rand.Int(rand.Reader, big.NewInt(int64(maxJitter)))
	delay := minDelay
	if err == nil {
		delay += time.Duration(n.Int64())
	}
	time.Sleep(delay)

	for i := 0; i < gSessionsNum; i++ {
		sessMu[i].Lock()
		if sessions[i] == s {
			sessions[i] = nil
			if gCloseOnRotate {
				log.Printf("Session rotated after %s — closing active connections", delay.Round(time.Second))
				go s.Close()
			} else {
				log.Printf("Session rotated after %s — next request will reconnect", delay.Round(time.Second))
			}
			sessMu[i].Unlock()
			break
		}
		sessMu[i].Unlock()
	}
}

// dropStalledSession kills the active session if a stream exceeds the idle timeout.
func dropStalledSession(s *yamux.Session) {
	for i := 0; i < gSessionsNum; i++ {
		sessMu[i].Lock()
		if sessions[i] == s {
			log.Printf("Session stalled during handshake, dropping to refresh connection")
			sessions[i] = nil
			go s.Close()
			sessMu[i].Unlock()
			break
		}
		sessMu[i].Unlock()
	}
}

type timeoutStream struct {
	net.Conn
}

func (ts *timeoutStream) Read(p []byte) (n int, err error) {
	if gConnectionsTimeOut > 0 {
		ts.Conn.SetReadDeadline(time.Now().Add(gConnectionsTimeOut))
	}
	n, err = ts.Conn.Read(p)
	if err != nil && gConnectionsTimeOut > 0 {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			go ts.Conn.Close()
			log.Printf("Stream idle for > %v, dropping", gConnectionsTimeOut)
		}
	}
	return
}

func (ts *timeoutStream) Write(p []byte) (n int, err error) {
	if gConnectionsTimeOut > 0 {
		ts.Conn.SetWriteDeadline(time.Now().Add(gConnectionsTimeOut))
	}
	n, err = ts.Conn.Write(p)
	if err != nil && gConnectionsTimeOut > 0 {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			go ts.Conn.Close()
			log.Printf("Stream idle for > %v, dropping", gConnectionsTimeOut)
		}
	}
	return
}

// openStream opens a new yamux stream and sends the tunnel destination.
// Returns a ready-to-use net.Conn for bidirectional data transfer.
// Retries once if the session has died, transparently reconnecting.
func openStream(destHost string, destPort uint16) (net.Conn, error) {
	for attempt := 0; attempt < 2; attempt++ {
		s, err := getSession()
		if err != nil {
			return nil, err
		}

		stream, err := s.Open()
		if err != nil {
			// Session may have died; clear it so getSession creates a fresh one.
			for i := 0; i < gSessionsNum; i++ {
				sessMu[i].Lock()
				if sessions[i] == s {
					sessions[i] = nil
				}
				sessMu[i].Unlock()
			}
			continue
		}

		// Send destination: [atyp][addr][port]
		var addrBytes []byte
		ip := net.ParseIP(destHost)
		if ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				addrBytes = append([]byte{0x01}, ip4...)
			} else {
				addrBytes = append([]byte{0x04}, ip.To16()...)
			}
		} else {
			if len(destHost) > 255 {
				go stream.Close()
				return nil, fmt.Errorf("domain name too long: %d bytes", len(destHost))
			}
			addrBytes = append([]byte{0x03, byte(len(destHost))}, []byte(destHost)...)
		}

		var portBytes [2]byte
		binary.BigEndian.PutUint16(portBytes[:], destPort)

		req := append([]byte{0x00}, addrBytes...)
		req = append(req, portBytes[:]...)
		if gConnectionsTimeOut > 0 {
			stream.SetWriteDeadline(time.Now().Add(gConnectionsTimeOut))
		}
		if _, err := stream.Write(req); err != nil {
			go stream.Close()
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				dropStalledSession(s)
			}
			return nil, fmt.Errorf("write tunnel request: %w", err)
		}

		// Read server response: 0x00 = ok, 0x01 = error
		var respBuf [1]byte
		if gConnectionsTimeOut > 0 {
			stream.SetReadDeadline(time.Now().Add(gConnectionsTimeOut))
		}
		if _, err := io.ReadFull(stream, respBuf[:]); err != nil {
			go stream.Close()
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				dropStalledSession(s)
			}
			return nil, fmt.Errorf("read tunnel response: %w", err)
		}
		if respBuf[0] != 0x00 {
			go stream.Close()
			return nil, fmt.Errorf("server rejected connection to %s:%d", destHost, destPort)
		}

		return &timeoutStream{Conn: stream}, nil
	}
	return nil, fmt.Errorf("failed to open stream after retry")
}

// openUDPStream opens a yamux stream for UDP relay and returns it after server ACK.
func openUDPStream() (net.Conn, error) {
	for attempt := 0; attempt < 2; attempt++ {
		s, err := getSession()
		if err != nil {
			return nil, err
		}
		stream, err := s.Open()
		if err != nil {
			for i := 0; i < gSessionsNum; i++ {
				sessMu[i].Lock()
				if sessions[i] == s {
					sessions[i] = nil
				}
				sessMu[i].Unlock()
			}
			continue
		}
		if gConnectionsTimeOut > 0 {
			stream.SetWriteDeadline(time.Now().Add(gConnectionsTimeOut))
		}
		if _, err := stream.Write([]byte{0x01}); err != nil {
			go stream.Close()
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				dropStalledSession(s)
			}
			return nil, fmt.Errorf("write UDP cmd: %w", err)
		}
		var ack [1]byte
		if gConnectionsTimeOut > 0 {
			stream.SetReadDeadline(time.Now().Add(gConnectionsTimeOut))
		}
		if _, err := io.ReadFull(stream, ack[:]); err != nil {
			go stream.Close()
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				dropStalledSession(s)
			}
			return nil, fmt.Errorf("read UDP ack: %w", err)
		}
		if ack[0] != 0x00 {
			go stream.Close()
			return nil, fmt.Errorf("server rejected UDP relay")
		}
		return &timeoutStream{Conn: stream}, nil
	}
	return nil, fmt.Errorf("failed to open UDP stream after retry")
}

// handleSocks5UDP handles a SOCKS5 UDP ASSOCIATE request.
// It opens a yamux UDP relay stream to the server, binds a local UDP socket,
// and proxies SOCKS5 UDP datagrams bidirectionally until the TCP control
// connection is closed by the client (RFC 1928).
func handleSocks5UDP(tcpConn net.Conn) {
	// Open relay stream first so we can signal failure before replying to client.
	stream, err := openUDPStream()
	if err != nil {
		tcpConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		log.Printf("UDP ASSOCIATE stream error: %v", err)
		return
	}
	defer func() { go stream.Close() }()

	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		tcpConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		log.Printf("UDP ASSOCIATE listen error: %v", err)
		return
	}
	defer func() { go udpConn.Close() }()

	udpAddr := udpConn.LocalAddr().(*net.UDPAddr)
	reply := []byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, byte(udpAddr.Port >> 8), byte(udpAddr.Port)}
	if _, err := tcpConn.Write(reply); err != nil {
		return
	}

	log.Printf("UDP relay active, local UDP: %s", udpAddr)

	var addrMu sync.Mutex
	var appAddr net.Addr

	// App → Server: receive SOCKS5 UDP datagrams, strip RSV(2)+FRAG(1) prefix,
	// and forward [ATYP+ADDR+PORT+DATA] as length-prefixed frames over the relay stream.
	go func() {
		buf := make([]byte, 65535)
		frameBuf := make([]byte, 4+65535) // persistent frame buffer: [4B len][payload]
		for {
			n, addr, err := udpConn.ReadFrom(buf)
			if err != nil {
				return
			}
			addrMu.Lock()
			appAddr = addr
			addrMu.Unlock()
			// RSV(2)+FRAG(1)+ATYP(1) = 4 bytes minimum; drop fragmented datagrams (FRAG!=0).
			if n < 4 || buf[2] != 0x00 {
				continue
			}
			payload := buf[3:n] // ATYP + ADDR + PORT + DATA
			// Reuse persistent frame buffer: [4B len][payload]
			binary.BigEndian.PutUint32(frameBuf[:4], uint32(len(payload)))
			copy(frameBuf[4:], payload)
			stream.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := stream.Write(frameBuf[:4+len(payload)]); err != nil {
				return
			}
		}
	}()

	// Server → App: receive length-prefixed frames, wrap in SOCKS5 UDP header,
	// and send to the app's UDP address.
	go func() {
		lenBuf := make([]byte, 4)
		pktBuf := make([]byte, 3+65535) // persistent: [RSV(2)+FRAG(1)][payload up to 65535]
		for {
			if _, err := io.ReadFull(stream, lenBuf); err != nil {
				go udpConn.Close()
				return
			}
			payloadLen := binary.BigEndian.Uint32(lenBuf)
			if payloadLen > 65535 {
				go udpConn.Close()
				return
			}
			payload := pktBuf[3 : 3+payloadLen] // read directly into pktBuf after the SOCKS5 header
			if _, err := io.ReadFull(stream, payload); err != nil {
				go udpConn.Close()
				return
			}
			addrMu.Lock()
			a := appAddr
			addrMu.Unlock()
			if a == nil {
				continue
			}
			// SOCKS5 UDP datagram: RSV(2)+FRAG(1)+[ATYP+ADDR+PORT+DATA]
			// Header bytes are already zero from make; payload already at pktBuf[3:].
			udpConn.WriteTo(pktBuf[:3+payloadLen], a)
		}
	}()

	// The association stays alive as long as the TCP control connection is open (RFC 1928).
	io.Copy(io.Discard, tcpConn)
}

// closeWrite signals the write-half of a connection is done, allowing the remote
// to flush any remaining data before the connection is fully torn down.
func closeWrite(c net.Conn) {
	type halfCloser interface{ CloseWrite() error }
	if hc, ok := c.(halfCloser); ok {
		hc.CloseWrite()
	}
}

// handleSocks5 handles an incoming SOCKS5 connection from a local application.
func handleSocks5(conn net.Conn) {
	defer func() { go conn.Close() }()

	cmd, host, port, err := socks5Handshake(conn)
	if err != nil {
		log.Printf("SOCKS5 handshake error from %s: %v", conn.RemoteAddr(), err)
		return
	}

	if cmd == 0x03 { // UDP ASSOCIATE
		handleSocks5UDP(conn)
		return
	}

	// Tell SOCKS5 client that connection succeeded BEFORE opening the stream
	// so that the client sends its first data payload, allowing us to peek it.
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	// Peek the first byte from the client application.
	pc, firstByte, err := peekOneByte(conn)
	if err != nil {
		log.Printf("Failed to peek first byte from %s: %v", conn.RemoteAddr(), err)
		return
	}

	// Use Vision framing if it looks like a TLS handshake (0x16).
	useVision := firstByte == 0x16

	var stream net.Conn
	if useVision {
		stream, err = openVisionStream(host, port)
	} else {
		stream, err = openStream(host, port)
	}
	if err != nil {
		log.Printf("Stream open error for %s:%d (vision=%v): %v", host, port, useVision, err)
		return
	}
	defer func() { go stream.Close() }()

	log.Printf("Tunneling %s → %s:%d (vision=%v)", conn.RemoteAddr(), host, port, useVision)

	done := make(chan struct{}, 2)

	if useVision {
		// Vision mode: encode upload (app→stream), decode download (stream→app).
		// Shaper upload throttle wraps the stream writer before passing to Vision.
		go func() {
			var dst io.Writer = stream
			if gShaper {
				dst = &shapedWriter{w: stream, bucket: &gUpBucket}
			}
			visionCopyToTunnel(dst, pc)
			closeWrite(stream)
			done <- struct{}{}
		}()
		go func() {
			visionCopyFromTunnel(pc, stream)
			closeWrite(pc)
			done <- struct{}{}
		}()
	} else {
		go func() {
			b := copyBufPool.Get().(*[]byte)
			var dst io.Writer = stream
			if gShaper {
				dst = &shapedWriter{w: stream, bucket: &gUpBucket}
			}
			io.CopyBuffer(dst, pc, *b)
			copyBufPool.Put(b)
			closeWrite(stream)
			done <- struct{}{}
		}()
		go func() {
			b := copyBufPool.Get().(*[]byte)
			io.CopyBuffer(pc, stream, *b)
			copyBufPool.Put(b)
			closeWrite(pc)
			done <- struct{}{}
		}()
	}

	<-done
	<-done
}

// socks5Handshake performs the SOCKS5 greeting + request exchange.
// Returns the command byte (0x01 CONNECT or 0x03 UDP ASSOCIATE) and
// destination host/port (populated for CONNECT; addr hint for UDP ASSOCIATE).
func socks5Handshake(conn net.Conn) (cmd byte, host string, port uint16, err error) {
	// --- Greeting ---
	header := make([]byte, 2)
	if _, err = io.ReadFull(conn, header); err != nil {
		return 0, "", 0, fmt.Errorf("read greeting: %w", err)
	}
	if header[0] != 0x05 {
		return 0, "", 0, fmt.Errorf("expected SOCKS5, got version %d", header[0])
	}
	methods := make([]byte, header[1])
	if _, err = io.ReadFull(conn, methods); err != nil {
		return 0, "", 0, fmt.Errorf("read methods: %w", err)
	}
	// Select no-auth
	if _, err = conn.Write([]byte{0x05, 0x00}); err != nil {
		return 0, "", 0, fmt.Errorf("write method selection: %w", err)
	}

	// --- Request ---
	req := make([]byte, 4)
	if _, err = io.ReadFull(conn, req); err != nil {
		return 0, "", 0, fmt.Errorf("read request: %w", err)
	}
	if req[0] != 0x05 {
		return 0, "", 0, fmt.Errorf("invalid request version: %d", req[0])
	}
	cmd = req[1]
	switch cmd {
	case 0x01: // CONNECT — TCP
	case 0x03: // UDP ASSOCIATE
		if !gUDPEnabled {
			conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			return 0, "", 0, fmt.Errorf("UDP ASSOCIATE disabled (--udp=false)")
		}
	default:
		conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return 0, "", 0, fmt.Errorf("unsupported SOCKS5 command: 0x%02x", cmd)
	}

	switch req[3] {
	case 0x01: // IPv4
		ip := make([]byte, 4)
		if _, err = io.ReadFull(conn, ip); err != nil {
			return 0, "", 0, fmt.Errorf("read IPv4: %w", err)
		}
		host = net.IP(ip).String()
	case 0x03: // domain
		lenBuf := make([]byte, 1)
		if _, err = io.ReadFull(conn, lenBuf); err != nil {
			return 0, "", 0, fmt.Errorf("read domain length: %w", err)
		}
		domain := make([]byte, lenBuf[0])
		if _, err = io.ReadFull(conn, domain); err != nil {
			return 0, "", 0, fmt.Errorf("read domain: %w", err)
		}
		host = string(domain)
	case 0x04: // IPv6
		ip := make([]byte, 16)
		if _, err = io.ReadFull(conn, ip); err != nil {
			return 0, "", 0, fmt.Errorf("read IPv6: %w", err)
		}
		host = net.IP(ip).String()
	default:
		conn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return 0, "", 0, fmt.Errorf("unsupported address type: 0x%02x", req[3])
	}

	portBuf := make([]byte, 2)
	if _, err = io.ReadFull(conn, portBuf); err != nil {
		return 0, "", 0, fmt.Errorf("read port: %w", err)
	}
	port = binary.BigEndian.Uint16(portBuf)
	return cmd, host, port, nil
}
