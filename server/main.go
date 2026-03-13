package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/xtls/reality"
)

func main() {
	port := flag.String("port", "443", "listening port")
	privKeyB64 := flag.String("private-key", "", "x25519 private key (base64, 32 bytes; generated if empty)")
	shortIdFlag := flag.String("short-id", "", "Reality short ID (hex, up to 16 chars; generated if empty)")
	destFlag := flag.String("dest", "cloudflare.com:443", "fallback destination for probes (host:port)")
	serverNamesFlag := flag.String("server-names", "", "comma-separated allowed SNIs (default: dest hostname)")
	debug := flag.Bool("debug", false, "enable Reality debug logging (verbose, for troubleshooting only)")
	flag.Parse()

	// x25519 private key
	privKeyBytes := make([]byte, 32)
	if *privKeyB64 == "" {
		if _, err := rand.Read(privKeyBytes); err != nil {
			log.Fatalf("generate private key: %v", err)
		}
		log.Printf("Generated private key — save this for --private-key on restart: %s", base64.StdEncoding.EncodeToString(privKeyBytes))
	} else {
		b, err := base64.StdEncoding.DecodeString(*privKeyB64)
		if err != nil || len(b) != 32 {
			log.Fatalf("--private-key must be base64 of exactly 32 bytes")
		}
		privKeyBytes = b
	}

	privKey, err := ecdh.X25519().NewPrivateKey(privKeyBytes)
	if err != nil {
		log.Fatalf("invalid x25519 private key: %v", err)
	}
	pubKeyBytes := privKey.PublicKey().Bytes()
	log.Printf("Public key (use as client --public-key): %s", base64.StdEncoding.EncodeToString(pubKeyBytes))

	// short ID
	var shortIdArr [8]byte
	if *shortIdFlag == "" {
		if _, err := rand.Read(shortIdArr[:]); err != nil {
			log.Fatalf("generate short ID: %v", err)
		}
		log.Printf("Short ID (use as client --short-id): %s", hex.EncodeToString(shortIdArr[:]))
	} else {
		b, err := hex.DecodeString(*shortIdFlag)
		if err != nil || len(b) > 8 {
			log.Fatalf("--short-id must be up to 16 hex chars")
		}
		copy(shortIdArr[:], b)
	}

	// dest and server names
	dest := *destFlag
	if _, _, err := net.SplitHostPort(dest); err != nil {
		dest = dest + ":443"
	}
	serverNames := map[string]bool{}
	if *serverNamesFlag != "" {
		for _, name := range strings.Split(*serverNamesFlag, ",") {
			if name = strings.TrimSpace(name); name != "" {
				serverNames[name] = true
			}
		}
	} else {
		host, _, _ := net.SplitHostPort(dest)
		if host == "" {
			host = dest
		}
		serverNames[host] = true
	}
	log.Printf("Allowed SNIs: %v", serverNames)

	realityConf := &reality.Config{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, address)
		},
		Type:                   "tcp",
		Dest:                   dest,
		ServerNames:            serverNames,
		PrivateKey:             privKeyBytes,
		ShortIds:               map[[8]byte]bool{shortIdArr: true},
		Show:                   *debug,
		SessionTicketsDisabled: true,
	}

	ln, err := net.Listen("tcp", ":"+*port)
	if err != nil {
		log.Fatalf("listen :%s: %v", *port, err)
	}
	log.Printf("Umbrella/REALITY server on :%s (fallback → %s)", *port, dest)

	realityLn := reality.NewListener(ln, realityConf)
	for {
		conn, err := realityLn.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handleConn(conn)
	}
}

// handleConn starts a yamux server session on an already-authenticated
// Reality connection and dispatches each stream to handleTunnel.
func handleConn(conn net.Conn) {
	defer conn.Close()

	muxSess, err := yamux.Server(conn, nil)
	if err != nil {
		log.Printf("yamux server %s: %v", conn.RemoteAddr(), err)
		return
	}
	defer muxSess.Close()

	log.Printf("Session from %s", conn.RemoteAddr())
	for {
		stream, err := muxSess.Accept()
		if err != nil {
			break
		}
		go func(s net.Conn) {
			defer s.Close()
			if err := handleTunnel(s); err != nil {
				log.Printf("tunnel: %v", err)
			}
		}(stream)
	}
}

// handleTunnel reads the tunnel protocol request from a yamux stream and proxies it.
//
// Request format:
//
//	[1 byte: address type] [address] [2 bytes: port, big-endian]
//
// Address types:
//
//	0x01 — IPv4  (4 bytes)
//	0x03 — domain (1 byte length + N bytes)
//	0x04 — IPv6  (16 bytes)
//
// Response: [1 byte: 0x00 = ok, 0x01 = error]
func handleTunnel(conn net.Conn) error {
	// Read address type
	atyp := make([]byte, 1)
	if _, err := io.ReadFull(conn, atyp); err != nil {
		return fmt.Errorf("read atyp: %w", err)
	}

	var host string
	switch atyp[0] {
	case 0x01: // IPv4
		ip := make([]byte, 4)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return fmt.Errorf("read IPv4: %w", err)
		}
		host = net.IP(ip).String()
	case 0x03: // domain
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return fmt.Errorf("read domain length: %w", err)
		}
		domain := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(conn, domain); err != nil {
			return fmt.Errorf("read domain: %w", err)
		}
		host = string(domain)
	case 0x04: // IPv6
		ip := make([]byte, 16)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return fmt.Errorf("read IPv6: %w", err)
		}
		host = net.IP(ip).String()
	default:
		conn.Write([]byte{0x01}) // error
		return fmt.Errorf("unsupported address type: 0x%02x", atyp[0])
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return fmt.Errorf("read port: %w", err)
	}
	target := fmt.Sprintf("%s:%d", host, binary.BigEndian.Uint16(portBuf))

	// Connect to destination
	remote, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		conn.Write([]byte{0x01}) // error
		return fmt.Errorf("dial %s: %w", target, err)
	}
	defer remote.Close()

	// Signal success — client can start sending data
	if _, err := conn.Write([]byte{0x00}); err != nil {
		return fmt.Errorf("write ok: %w", err)
	}

	log.Printf("Proxying %s → %s", conn.RemoteAddr(), target)

	// Bidirectional copy
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(remote, conn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(conn, remote)
		done <- struct{}{}
	}()
	<-done

	return nil
}
