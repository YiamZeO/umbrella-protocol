package xtls

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/big"
	mrand "math/rand"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/miekg/dns"
	utls "github.com/refraction-networking/utls"

	"umbrella_client/internal/client/config"
	"umbrella_client/internal/client/shaper"
	"umbrella_client/internal/client/share"
	"umbrella_client/internal/client/umbrella_dns"
	"umbrella_client/internal/storage"
)

// Global session state — all SOCKS5 connections share one TLS connection.
var (
	gServerAddr         string
	gSNI                string
	gServerPubKey       []byte
	gShortId            [8]byte
	gUDPEnabled         bool
	gShaper             bool
	gConnectionsTimeOut time.Duration
	gSessionsNum        int
	gBypass             []string
	gDNSListen          string
	gDNSUpstream        string
	sessMu              []sync.Mutex
	getSessionMu        sync.Mutex // Global lock to prevent simultaneous session establishment
	sessions            []*yamux.Session
	wg                  sync.WaitGroup
)

func Start(cfg *config.Config, ctx context.Context, appFilesDir string, dnsCache *storage.DnsCache) error {
	mrand.Seed(time.Now().UnixNano())

	gUDPEnabled = cfg.UDPEnabled
	gShaper = cfg.Shaper
	gConnectionsTimeOut = time.Duration(cfg.Xtls.ConnectionsTimeOut) * time.Second
	gSessionsNum = cfg.Xtls.SessionsNum
	gBypass = make([]string, len(cfg.Bypass))
	copy(gBypass, cfg.Bypass)
	gDNSListen = cfg.DNSListen
	gDNSUpstream = cfg.DNSUpstream

	pubKey, err := base64.StdEncoding.DecodeString(cfg.Xtls.PublicKey)
	if err != nil || len(pubKey) != 32 {
		return fmt.Errorf("invalid public-key in config: must be base64 of exactly 32 bytes")
	}

	shortIdBytes, err := hex.DecodeString(cfg.Xtls.ShortId)
	if err != nil || len(shortIdBytes) > 8 {
		return fmt.Errorf("invalid short-id in config: must be up to 16 hex chars")
	}

	gServerAddr = cfg.Server
	gSNI = cfg.SNI
	gServerPubKey = pubKey
	copy(gShortId[:], shortIdBytes)

	// Automatically bypass the server's own address to avoid loops and allow direct access to VPS services.
	if host, _, err := net.SplitHostPort(gServerAddr); err == nil {
		gBypass = append(gBypass, host)
	} else {
		gBypass = append(gBypass, gServerAddr)
	}

	// Automatically bypass loopback addresses.
	// This ensures that local applications (like Mihomo sending DNS queries to 127.0.0.1:53 via SOCKS5)
	// correctly hit our local DNS server instead of tunneling 127.0.0.1 to the remote VPS.
	gBypass = append(gBypass,
		"127.0.0.0/8",
		"::1/128",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"fc00::/7",
		"fe80::/10",
	)

	if gSessionsNum < 1 {
		gSessionsNum = 1
	}
	sessMu = make([]sync.Mutex, gSessionsNum)
	sessions = make([]*yamux.Session, gSessionsNum)

	if gShaper {
		if cfg.PhasesData != nil {
			if err := shaper.ParsePhases(cfg.PhasesData); err != nil {
				return fmt.Errorf("failed to parse embedded phases: %v", err)
			}
			log.Printf("[INFO] Loaded %d phases from embedded data", len(shaper.Phases))
		} else {
			if err := shaper.LoadPhases(cfg.PhasesFile); err != nil {
				return fmt.Errorf("failed to load phases from %s: %v", cfg.PhasesFile, err)
			}
			log.Printf("[INFO] Loaded %d phases from %s", len(shaper.Phases), cfg.PhasesFile)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			shaper.RunShaperEngine(ctx)
		}()
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %v", cfg.ListenAddr, err)
	}
	log.Printf("[INFO] Umbrella/Reality client on %s (SOCKS5) → %s (SNI: %s)", cfg.ListenAddr, cfg.Server, cfg.SNI)

	if gDNSListen != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			umbrella_dns.RunDNSServer(ctx, dnsCache, gBypass, gDNSListen, gDNSUpstream, forwardDNS)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		ln.Close()
		for i := 0; i < gSessionsNum; i++ {
			sessMu[i].Lock()
			if sessions[i] != nil {
				go sessions[i].Close()
				sessions[i] = nil
			}
			sessMu[i].Unlock()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[INFO] Umbrella/Reality client listener closed, stopping")
			ch := make(chan bool)
			go func() {
				wg.Wait()
				ch <- true
			}()
			select {
			case <-ch:
				return nil
			case <-time.After(5 * time.Second):
				return nil
			}
		default:
			// Set a deadline so Accept() can timeout and check context
			if tcpLn, ok := ln.(*net.TCPListener); ok {
				tcpLn.SetDeadline(time.Now().Add(5 * time.Second))
			}

			conn, err := ln.Accept()
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// Timeout is expected, continue to check context
					continue
				}
				// Other errors
				log.Printf("[ERR] Accept error: %v", err)
				continue
			}

			// Reset deadline for the connection
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				tcpConn.SetDeadline(time.Time{})
			}

			wg.Add(1)
			go func() {
				defer wg.Done()
				handleSocks5(ctx, conn, dnsCache)
			}()
		}
	}
}

// forwardDNS sends a DNS query to the upstream server via the Umbrella tunnel.
func forwardDNS(ctx context.Context, r *dns.Msg) (*dns.Msg, error) {
	s, err := getSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}

	stream, err := openVisionUDPStream(s)
	if err != nil {
		return nil, fmt.Errorf("open UDP stream: %w", err)
	}
	defer stream.Close()

	// Prepare length-prefixed UDP frame for our relay protocol:
	// [4B len][ATYP+ADDR+PORT+DATA]
	dnsData, err := r.Pack()
	if err != nil {
		return nil, fmt.Errorf("pack dns: %w", err)
	}

	host, portStr, _ := net.SplitHostPort(gDNSUpstream)
	var port uint16
	fmt.Sscanf(portStr, "%d", &port)

	// Build relay header: [ATYP][ADDR][PORT]
	var addrBytes []byte
	ip := net.ParseIP(host)
	if ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			addrBytes = append([]byte{0x01}, ip4...)
		} else {
			addrBytes = append([]byte{0x04}, ip.To16()...)
		}
	} else {
		addrBytes = append([]byte{0x03, byte(len(host))}, []byte(host)...)
	}
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], port)

	payload := append(addrBytes, portBytes[:]...)
	payload = append(payload, dnsData...)

	if err := visionWriteDatagram(stream, payload); err != nil {
		return nil, err
	}

	// Read response
	respPayload, err := visionReadDatagram(stream)
	if err != nil {
		return nil, err
	}

	// Skip ATYP+ADDR+PORT in response (server returns them)
	// Response from server is [ATYP][ADDR][PORT][DATA]
	off := 0
	switch respPayload[0] {
	case 0x01:
		off = 1 + 4 + 2
	case 0x04:
		off = 1 + 16 + 2
	case 0x03:
		off = 1 + 1 + int(respPayload[1]) + 2
	}

	respMsg := new(dns.Msg)
	if err := respMsg.Unpack(respPayload[off:]); err != nil {
		return nil, fmt.Errorf("unpack dns resp: %w", err)
	}

	return respMsg, nil
}

// getSession returns a multiplexed session from the pool, creating one if needed.
// All SOCKS5 connections share a pool of TLS connections via yamux streams.
// The TLS handshake uses the Reality protocol: authentication is embedded into
// the ClientHello via ECDH + AES-GCM-encrypted SessionID, making the connection
// indistinguishable from a real TLS handshake to a legitimate site.
func getSession(ctx context.Context) (*yamux.Session, error) {
	// Global lock to prevent simultaneous session establishment (DDoS protection)
	getSessionMu.Lock()
	defer getSessionMu.Unlock()

	// 1. Try the randomly selected slot first to maintain pool distribution
	idx := mrand.Intn(gSessionsNum)
	if sessions[idx] != nil && !sessions[idx].IsClosed() {
		return sessions[idx], nil
	}

	// 2. If the selected slot is empty/closed, try to establish it once
	sess, err := establishSession(ctx, idx)
	if err == nil {
		return sess, nil
	}
	log.Printf("[ERR] Failed to establish session in preferred slot %d: %v", idx, err)

	// 3. Fallback: Search for ANY already open session in the pool
	for i := 0; i < gSessionsNum; i++ {
		if i == idx {
			continue
		}
		if sessions[i] != nil && !sessions[i].IsClosed() {
			return sessions[i], nil
		}
	}

	// 4. Last resort: Try establishing a session in other slots with retries
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		// Small delay between retries to avoid triggering VPS DDoS protection
		select {
		case <-time.After(time.Duration(attempt+1) * 300 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		nextIdx := (idx + 1 + attempt) % gSessionsNum
		sess, err := establishSession(ctx, nextIdx)
		if err == nil {
			return sess, nil
		}
		lastErr = err
		log.Printf("[ERR] Fallback session attempt %d failed: %v", attempt+1, err)
	}

	return nil, fmt.Errorf("all session attempts failed: %w", lastErr)
}

func establishSession(ctx context.Context, idx int) (*yamux.Session, error) {
	sessMu[idx].Lock()
	defer sessMu[idx].Unlock()

	// Double-check if someone else opened it while we were waiting for the lock
	if sessions[idx] != nil && !sessions[idx].IsClosed() {
		return sessions[idx], nil
	}

	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 10 * time.Second,
	}
	tcpConn, err := dialer.DialContext(ctx, "tcp", gServerAddr)
	if err != nil {
		return nil, fmt.Errorf("tcp dial (%s): %w", gServerAddr, err)
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
	uConn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := uConn.Handshake(); err != nil {
		uConn.Close()
		return nil, fmt.Errorf("tls handshake (%s): %w", gSNI, err)
	}
	uConn.SetDeadline(time.Time{}) // clear deadline

	muxCfg := yamux.DefaultConfig()
	muxCfg.MaxStreamWindowSize = 8 * 1024 * 1024
	muxCfg.EnableKeepAlive = true
	muxCfg.StreamCloseTimeout = 10 * time.Second
	muxCfg.LogOutput = io.Discard
	sess, err := yamux.Client(uConn, muxCfg)
	if err != nil {
		uConn.Close()
		return nil, fmt.Errorf("yamux client: %w", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		scheduleReconnect(ctx, sess)
	}()
	sessions[idx] = sess
	log.Printf("[INFO] Reality session established to %s (slot %d)", gServerAddr, idx)
	return sess, nil
}

// scheduleReconnect waits a random interval (3–15 min) then removes the session
// from global state. Active yamux streams finish naturally; the next SOCKS5
// request will transparently create a fresh TLS connection with a new handshake.
func scheduleReconnect(ctx context.Context, s *yamux.Session) {
	// Random delay in [3, 15) minutes using crypto/rand for unpredictability.
	const minDelay = 3 * time.Minute
	const maxJitter = 12 * time.Minute
	n, err := rand.Int(rand.Reader, big.NewInt(int64(maxJitter)))
	delay := minDelay
	if err == nil {
		delay += time.Duration(n.Int64())
	}

	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return
	}

	for i := 0; i < gSessionsNum; i++ {
		sessMu[i].Lock()
		if sessions[i] == s {
			sessions[i] = nil
			go func() {
				time.Sleep(5 * time.Minute)
				s.Close()
			}()
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
			log.Printf("[ERR] Session stalled during handshake, dropping to refresh connection")
			sessions[i] = nil
			go s.Close()
			sessMu[i].Unlock()
			break
		}
		sessMu[i].Unlock()
	}
}

type timeoutConn struct {
	net.Conn
}

func (ts *timeoutConn) CloseWrite() error {
	type halfCloser interface{ CloseWrite() error }
	if hc, ok := ts.Conn.(halfCloser); ok {
		return hc.CloseWrite()
	}
	return nil
}

func (ts *timeoutConn) Read(p []byte) (n int, err error) {
	if gConnectionsTimeOut > 0 {
		ts.Conn.SetReadDeadline(time.Now().Add(gConnectionsTimeOut))
	}
	n, err = ts.Conn.Read(p)
	return
}

func (ts *timeoutConn) Write(p []byte) (n int, err error) {
	if gConnectionsTimeOut > 0 {
		ts.Conn.SetWriteDeadline(time.Now().Add(gConnectionsTimeOut))
	}
	n, err = ts.Conn.Write(p)
	return
}

// openVisionUDPStream opens a yamux stream for Vision-UDP relay and returns it after server ACK.
// Uses cmd=0x01 to indicate Vision framing for UDP datagrams.
func openVisionUDPStream(s *yamux.Session) (net.Conn, error) {
	stream, err := s.Open()
	if err != nil {
		for i := 0; i < gSessionsNum; i++ {
			sessMu[i].Lock()
			if sessions[i] == s {
				sessions[i] = nil
			}
			sessMu[i].Unlock()
		}
		return nil, err
	}
	if gConnectionsTimeOut > 0 {
		stream.SetWriteDeadline(time.Now().Add(gConnectionsTimeOut))
	}
	if _, err := stream.Write([]byte{0x01}); err != nil {
		go stream.Close()
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			dropStalledSession(s)
		}
		return nil, fmt.Errorf("write Vision UDP cmd: %w", err)
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
		return nil, fmt.Errorf("read Vision UDP ack: %w", err)
	}
	if ack[0] != 0x00 {
		go stream.Close()
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			dropStalledSession(s)
		}
		return nil, fmt.Errorf("server rejected Vision UDP relay")
	}
	return &timeoutConn{Conn: stream}, nil
}

type timeoutPacketConn struct {
	net.PacketConn
}

func (ts *timeoutPacketConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	if gConnectionsTimeOut > 0 {
		ts.PacketConn.SetReadDeadline(time.Now().Add(gConnectionsTimeOut))
	}
	n, addr, err = ts.PacketConn.ReadFrom(p)
	return
}

func (ts *timeoutPacketConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	if gConnectionsTimeOut > 0 {
		ts.PacketConn.SetWriteDeadline(time.Now().Add(gConnectionsTimeOut))
	}
	n, err = ts.PacketConn.WriteTo(p, addr)
	return
}

// handleSocks5UDP handles a SOCKS5 UDP ASSOCIATE request.
// It opens a yamux UDP relay stream to the server, binds a local UDP socket,
// and proxies SOCKS5 UDP datagrams bidirectionally until the TCP control
// connection is closed by the client (RFC 1928).
// Uses Vision framing for all UDP datagrams.
func handleSocks5UDP(ctx context.Context, tcpConn net.Conn, dnsCache *storage.DnsCache) {
	var (
		err    error
		s      *yamux.Session
		stream net.Conn
	)
	for range 3 {
		s, err = getSession(ctx)
		if err != nil {
			log.Printf("[ERR] UDP getSession error: %v", err)
			tcpConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			return
		}
		stream, err = openVisionUDPStream(s)
		if err != nil {
			log.Printf("[ERR] Vision UDP ASSOCIATE stream error: %v", err)
			continue
		} else {
			break
		}
	}

	if err != nil {
		tcpConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	defer func() {
		go stream.Close()
	}()

	rawUdpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		tcpConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		log.Printf("[ERR] UDP ASSOCIATE listen error: %v", err)
		return
	}

	udpConn := &timeoutPacketConn{PacketConn: rawUdpConn}
	defer func() { go udpConn.Close() }()

	// Жизненный цикл UDP релея
	udpCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	udpAddr := udpConn.LocalAddr().(*net.UDPAddr)
	reply := []byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, byte(udpAddr.Port >> 8), byte(udpAddr.Port)}
	if _, err := tcpConn.Write(reply); err != nil {
		return
	}

	log.Printf("[INFO] UDP relay active, local UDP: %s", udpAddr)

	var addrMu sync.Mutex
	var appAddr net.Addr

	var relayWriter io.Writer = stream
	var relayReader io.Reader = stream
	if gShaper {
		relayWriter = &shaper.ShapedWriter{W: stream, Bucket: &shaper.GUpBucket}
		relayReader = &shaper.ShapedReader{R: stream, Bucket: &shaper.GDownBucket}
	}

	// App → Server (Dynamic routing)
	go func() {
		defer cancel()
		bufPtr := share.UDPBufPool.Get().(*[]byte)
		defer share.UDPBufPool.Put(bufPtr)
		buf := *bufPtr

		frameBufPtr := share.UDPBufPool.Get().(*[]byte)
		defer share.UDPBufPool.Put(frameBufPtr)

		for {
			select {
			case <-ctx.Done():
			case <-udpCtx.Done():
				return
			default:
			}
			n, addr, err := udpConn.ReadFrom(buf)
			if err != nil {
				return
			}

			// If it's from a remote address (direct bypass response), it's handled in the other goroutine.
			// But since we use one udpConn, we need to distinguish.
			addrMu.Lock()
			if appAddr == nil {
				appAddr = addr
			}
			isApp := addr.String() == appAddr.String()
			addrMu.Unlock()

			if !isApp {
				// This is a direct response from a bypassed host.
				// Wrap it in SOCKS5 and send to the app.
				addrMu.Lock()
				app := appAddr
				addrMu.Unlock()
				if app != nil {
					// Extract source address from the incoming packet (addr)
					udpSrc := addr.(*net.UDPAddr)
					resp := []byte{0, 0, 0} // RSV(2), FRAG(1)
					if ip4 := udpSrc.IP.To4(); ip4 != nil {
						resp = append(resp, 0x01)
						resp = append(resp, ip4...)
					} else {
						resp = append(resp, 0x04)
						resp = append(resp, udpSrc.IP.To16()...)
					}
					var p [2]byte
					binary.BigEndian.PutUint16(p[:], uint16(udpSrc.Port))
					resp = append(resp, p[:]...)
					resp = append(resp, buf[:n]...)
					udpConn.WriteTo(resp, app)
				}
				continue
			}

			// This is from the app. Parse destination.
			if n < 4 || buf[2] != 0x00 {
				continue
			}

			host, port, dataStart, err := parseSOCKS5UDPAddr(buf)
			if err != nil {
				continue
			}

			// Try DNS cache
			var domainFromIp string
			if c, ok := dnsCache.Load(host); ok {
				domainFromIp = c.(string)
				log.Printf("[INFO] Resolved %s → %s (from DNS cache)", host, domainFromIp)
			}

			var isShouldBypass bool
			if len(domainFromIp) > 0 {
				isShouldBypass = umbrella_dns.ShouldBypass(domainFromIp, gBypass)
			} else {
				isShouldBypass = umbrella_dns.ShouldBypass(host, gBypass)
			}

			if isShouldBypass {
				// Bypass! Send directly.
				targetAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
				if err == nil {
					udpConn.WriteTo(buf[dataStart:n], targetAddr)
				}
				continue
			}

			// Not bypass. Send through tunnel with Vision framing.
			payload := buf[3:n] // ATYP + ADDR + PORT + DATA
			if err := visionWriteDatagram(relayWriter, payload); err != nil {
				return
			}
		}
	}()

	// Server → App (Tunnel responses)
	go func() {
		defer cancel()
		pktBufPtr := share.UDPBufPool.Get().(*[]byte)
		defer share.UDPBufPool.Put(pktBufPtr)
		pktBuf := *pktBufPtr
		for {
			select {
			case <-ctx.Done():
			case <-udpCtx.Done():
				return
			default:
			}
			payload, err := visionReadDatagram(relayReader)
			if err != nil {
				return
			}
			addrMu.Lock()
			a := appAddr
			addrMu.Unlock()
			if a == nil {
				continue
			}
			udpConn.WriteTo(pktBuf[:3+len(payload)], a)
		}
	}()

	// Ждем закрытия контекста или TCP соединения
	<-udpCtx.Done()
}

// handleSocks5 handles an incoming SOCKS5 connection from a local application.
func handleSocks5(ctx context.Context, conn net.Conn, dnsCache *storage.DnsCache) {
	conn = &timeoutConn{Conn: conn}

	defer func() {
		go conn.Close()
	}()

	cmd, host, port, err := share.Socks5Handshake(conn, gUDPEnabled)
	if err != nil {
		log.Printf("[ERR] SOCKS5 handshake error from %s: %v", conn.RemoteAddr(), err)
		return
	}

	// Try DNS cache
	var domainFromIp string
	if c, ok := dnsCache.Load(host); ok {
		domainFromIp = c.(string)
		log.Printf("[INFO] Resolved %s → %s (from DNS cache)", host, domainFromIp)
	}

	if cmd == 0x03 { // UDP ASSOCIATE
		handleSocks5UDP(ctx, conn, dnsCache)
		return
	}

	var isShouldBypass bool
	if len(domainFromIp) > 0 {
		isShouldBypass = umbrella_dns.ShouldBypass(domainFromIp, gBypass)
	} else {
		isShouldBypass = umbrella_dns.ShouldBypass(host, gBypass)
	}

	if isShouldBypass {
		conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		share.HandleDirect(ctx, conn, host, port)
		return
	}

	// Tell SOCKS5 client that connection succeeded BEFORE opening the stream
	// so that the client sends its first data payload, allowing us to peek it.
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	target := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	var (
		stream net.Conn
		s      *yamux.Session
	)
	for range 3 {
		s, err = getSession(ctx)
		if err != nil {
			log.Printf("[ERR] getSession error for %s → %v", target, err)
			return
		}
		stream, err = openVisionStream(s, host, port)
		if err != nil {
			log.Printf("[ERR] Stream open error for %s → %v", target, err)
		} else {
			break
		}
	}

	if err != nil {
		return
	}

	defer func() {
		go stream.Close()
	}()

	log.Printf("[INFO] Tunneling %s → %s", conn.RemoteAddr(), target)

	sCtx, sCancel := context.WithCancel(ctx)
	defer sCancel()

	// Relay data with half-close support
	go func() {
		b := share.CopyBufPool.Get().(*[]byte)
		defer share.CopyBufPool.Put(b)
		var dst io.Writer = stream
		if gShaper {
			dst = &shaper.ShapedWriter{W: stream, Bucket: &shaper.GUpBucket}
		}
		io.CopyBuffer(dst, conn, *b)
		share.CloseWrite(stream)
	}()

	go func() {
		defer sCancel()
		b := share.CopyBufPool.Get().(*[]byte)
		defer share.CopyBufPool.Put(b)
		var src io.Reader = stream
		if gShaper {
			src = &shaper.ShapedReader{R: stream, Bucket: &shaper.GDownBucket}
		}
		io.CopyBuffer(conn, src, *b)
	}()

	select {
	case <-sCtx.Done():
	case <-ctx.Done():
	}
}

// parseSOCKS5UDPAddr parses the destination address from a SOCKS5 UDP datagram.
// Returns the host, port, and the starting index of the actual data.
func parseSOCKS5UDPAddr(buf []byte) (host string, port uint16, dataStart int, err error) {
	if len(buf) < 4 {
		return "", 0, 0, fmt.Errorf("packet too short")
	}
	// buf[0:2] RSV, buf[2] FRAG
	atyp := buf[3]
	switch atyp {
	case 0x01: // IPv4
		if len(buf) < 10 {
			return "", 0, 0, fmt.Errorf("short IPv4 packet")
		}
		host = net.IP(buf[4:8]).String()
		port = binary.BigEndian.Uint16(buf[8:10])
		dataStart = 10
	case 0x03: // Domain
		hostLen := int(buf[4])
		if len(buf) < 5+hostLen+2 {
			return "", 0, 0, fmt.Errorf("short domain packet")
		}
		host = string(buf[5 : 5+hostLen])
		port = binary.BigEndian.Uint16(buf[5+hostLen : 5+hostLen+2])
		dataStart = 5 + hostLen + 2
	case 0x04: // IPv6
		if len(buf) < 22 {
			return "", 0, 0, fmt.Errorf("short IPv6 packet")
		}
		host = net.IP(buf[4:20]).String()
		port = binary.BigEndian.Uint16(buf[20:22])
		dataStart = 22
	default:
		return "", 0, 0, fmt.Errorf("unsupported atyp: 0x%02x", atyp)
	}
	return
}
