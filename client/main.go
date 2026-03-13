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
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	utls "github.com/refraction-networking/utls"
)

// Global session state — all SOCKS5 connections share one TLS connection.
var (
	gServerAddr   string
	gSNI          string
	gServerPubKey []byte
	gShortId      [8]byte

	sessMu sync.Mutex
	sess   *yamux.Session
)

func main() {
	serverAddr := flag.String("server", "", "server address, e.g. vps.example.com:443 (required)")
	publicKeyB64 := flag.String("public-key", "", "server x25519 public key in base64 (required)")
	shortIdHex := flag.String("short-id", "", "Reality short ID in hex, up to 16 chars (required)")
	sni := flag.String("sni", "cloudflare.com", "SNI to use in TLS Client Hello")
	listenAddr := flag.String("listen", "127.0.0.1:1080", "local SOCKS5 listen address")
	flag.Parse()

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

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Accept error: %v", err)
			continue
		}
		go handleSocks5(conn)
	}
}

// getSession returns the current multiplexed session, creating one if needed.
// All SOCKS5 connections share a single TLS connection via yamux streams.
// The TLS handshake uses the REALITY protocol: authentication is embedded into
// the ClientHello via ECDH + AES-GCM-encrypted SessionID, making the connection
// indistinguishable from a real TLS handshake to a legitimate site.
func getSession() (*yamux.Session, error) {
	sessMu.Lock()
	defer sessMu.Unlock()

	if sess != nil && !sess.IsClosed() {
		return sess, nil
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
		tcpConn.Close()
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
		uConn.Close()
		return nil, fmt.Errorf("no x25519 key share in ClientHello")
	}

	// ECDH: shared = X25519(clientEphemeralPriv, serverStaticPub)
	serverPub, err := ecdh.X25519().NewPublicKey(gServerPubKey)
	if err != nil {
		uConn.Close()
		return nil, fmt.Errorf("parse server public key: %w", err)
	}
	sharedSecret, err := ephPriv.ECDH(serverPub)
	if err != nil {
		uConn.Close()
		return nil, fmt.Errorf("ecdh: %w", err)
	}

	// authKey = HKDF-SHA256(ikm=sharedSecret, salt=random[:20], info="REALITY")
	random := uConn.HandshakeState.Hello.Random
	authKey, err := hkdf.Key(sha256.New, sharedSecret, random[:20], "REALITY", 32)
	if err != nil {
		uConn.Close()
		return nil, fmt.Errorf("hkdf: %w", err)
	}

	// Build AAD: marshal the ClientHello with sessionId = zeros.
	// The server decrypts using the same marshaled form as additional data.
	uConn.HandshakeState.Hello.SessionId = make([]byte, 32) // zero sessionId
	if err := uConn.MarshalClientHello(); err != nil {
		uConn.Close()
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
		uConn.Close()
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		uConn.Close()
		return nil, fmt.Errorf("aes gcm: %w", err)
	}
	encSessionId := aead.Seal(nil, random[20:32], plaintext, aad)

	// Set the encrypted sessionId and re-marshal.
	uConn.HandshakeState.Hello.SessionId = encSessionId
	if err := uConn.MarshalClientHello(); err != nil {
		uConn.Close()
		return nil, fmt.Errorf("marshal (encrypted sessionId): %w", err)
	}

	// Complete TLS handshake — sends the modified ClientHello.
	if err := uConn.Handshake(); err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("tls handshake: %w", err)
	}

	newSess, err := yamux.Client(uConn, nil)
	if err != nil {
		uConn.Close()
		return nil, fmt.Errorf("yamux client: %w", err)
	}

	sess = newSess
	go scheduleReconnect(newSess)
	log.Printf("REALITY session established to %s", gServerAddr)
	return sess, nil
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

	sessMu.Lock()
	if sess == s {
		sess = nil
		log.Printf("Session rotated after %s — next request will reconnect", delay.Round(time.Second))
	}
	sessMu.Unlock()
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
			sessMu.Lock()
			if sess == s {
				sess = nil
			}
			sessMu.Unlock()
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
				stream.Close()
				return nil, fmt.Errorf("domain name too long: %d bytes", len(destHost))
			}
			addrBytes = append([]byte{0x03, byte(len(destHost))}, []byte(destHost)...)
		}

		var portBytes [2]byte
		binary.BigEndian.PutUint16(portBytes[:], destPort)

		req := append(addrBytes, portBytes[:]...)
		if _, err := stream.Write(req); err != nil {
			stream.Close()
			return nil, fmt.Errorf("write tunnel request: %w", err)
		}

		// Read server response: 0x00 = ok, 0x01 = error
		var respBuf [1]byte
		if _, err := io.ReadFull(stream, respBuf[:]); err != nil {
			stream.Close()
			return nil, fmt.Errorf("read tunnel response: %w", err)
		}
		if respBuf[0] != 0x00 {
			stream.Close()
			return nil, fmt.Errorf("server rejected connection to %s:%d", destHost, destPort)
		}

		return stream, nil
	}
	return nil, fmt.Errorf("failed to open stream after retry")
}

// handleSocks5 handles an incoming SOCKS5 connection from a local application.
func handleSocks5(conn net.Conn) {
	defer conn.Close()

	host, port, err := socks5Handshake(conn)
	if err != nil {
		log.Printf("SOCKS5 handshake error from %s: %v", conn.RemoteAddr(), err)
		return
	}

	stream, err := openStream(host, port)
	if err != nil {
		conn.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		log.Printf("Stream open error for %s:%d: %v", host, port, err)
		return
	}
	defer stream.Close()

	// Tell SOCKS5 client that connection succeeded
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	log.Printf("Tunneling %s → %s:%d", conn.RemoteAddr(), host, port)

	done := make(chan struct{}, 2)
	go func() {
		io.Copy(stream, conn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(conn, stream)
		done <- struct{}{}
	}()
	<-done
}

// socks5Handshake performs the SOCKS5 greeting + request exchange.
// Returns the destination host and port requested by the client.
func socks5Handshake(conn net.Conn) (host string, port uint16, err error) {
	// --- Greeting ---
	header := make([]byte, 2)
	if _, err = io.ReadFull(conn, header); err != nil {
		return "", 0, fmt.Errorf("read greeting: %w", err)
	}
	if header[0] != 0x05 {
		return "", 0, fmt.Errorf("expected SOCKS5, got version %d", header[0])
	}
	methods := make([]byte, header[1])
	if _, err = io.ReadFull(conn, methods); err != nil {
		return "", 0, fmt.Errorf("read methods: %w", err)
	}
	// Select no-auth
	if _, err = conn.Write([]byte{0x05, 0x00}); err != nil {
		return "", 0, fmt.Errorf("write method selection: %w", err)
	}

	// --- Request ---
	req := make([]byte, 4)
	if _, err = io.ReadFull(conn, req); err != nil {
		return "", 0, fmt.Errorf("read request: %w", err)
	}
	if req[0] != 0x05 {
		return "", 0, fmt.Errorf("invalid request version: %d", req[0])
	}
	if req[1] != 0x01 {
		conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return "", 0, fmt.Errorf("unsupported SOCKS5 command: %d", req[1])
	}

	switch req[3] {
	case 0x01: // IPv4
		ip := make([]byte, 4)
		if _, err = io.ReadFull(conn, ip); err != nil {
			return "", 0, fmt.Errorf("read IPv4: %w", err)
		}
		host = net.IP(ip).String()
	case 0x03: // domain
		lenBuf := make([]byte, 1)
		if _, err = io.ReadFull(conn, lenBuf); err != nil {
			return "", 0, fmt.Errorf("read domain length: %w", err)
		}
		domain := make([]byte, lenBuf[0])
		if _, err = io.ReadFull(conn, domain); err != nil {
			return "", 0, fmt.Errorf("read domain: %w", err)
		}
		host = string(domain)
	case 0x04: // IPv6
		ip := make([]byte, 16)
		if _, err = io.ReadFull(conn, ip); err != nil {
			return "", 0, fmt.Errorf("read IPv6: %w", err)
		}
		host = net.IP(ip).String()
	default:
		conn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return "", 0, fmt.Errorf("unsupported address type: 0x%02x", req[3])
	}

	portBuf := make([]byte, 2)
	if _, err = io.ReadFull(conn, portBuf); err != nil {
		return "", 0, fmt.Errorf("read port: %w", err)
	}
	port = binary.BigEndian.Uint16(portBuf)
	return host, port, nil
}
