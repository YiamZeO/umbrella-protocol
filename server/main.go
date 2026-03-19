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
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/xtls/reality"
)

// copyBufPool holds reusable 32 KiB buffers for bidirectional TCP proxying.
var copyBufPool = sync.Pool{New: func() any { b := make([]byte, 32*1024); return &b }}

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

	muxCfg := yamux.DefaultConfig()
	muxCfg.MaxStreamWindowSize = 4 * 1024 * 1024
	// Telegram relies on long-lived idle connections for push events.
	muxCfg.EnableKeepAlive = true
	muxCfg.KeepAliveInterval = 5 * time.Minute
	muxCfg.ConnectionWriteTimeout = 30 * time.Minute
	muxSess, err := yamux.Server(conn, muxCfg)
	if err != nil {
		log.Printf("yamux server %s: %v", conn.RemoteAddr(), err)
		return
	}
	defer muxSess.Close()

	log.Printf("Session from %s", conn.RemoteAddr())
	var shaper atomic.Pointer[sessionShaper]
	for {
		stream, err := muxSess.Accept()
		if err != nil {
			break
		}
		go func(s net.Conn) {
			defer s.Close()
			cmdBuf := make([]byte, 1)
			if _, err := io.ReadFull(s, cmdBuf); err != nil {
				log.Printf("read stream cmd from %s: %v", conn.RemoteAddr(), err)
				return
			}
			var streamErr error
			switch cmdBuf[0] {
			case 0x00:
				streamErr = handleTunnel(s, shaper.Load())
			case 0x01:
				streamErr = handleUDPRelay(s)
			case 0x03:
				streamErr = handleVisionTunnel(s, shaper.Load())
			case 0x02:
				sh := &sessionShaper{}
				shaper.Store(sh)
				sh.runPhaseEngine(s)
				return
			default:
				log.Printf("unknown stream cmd 0x%02x from %s", cmdBuf[0], conn.RemoteAddr())
				return
			}
			if streamErr != nil {
				log.Printf("stream error from %s: %v", conn.RemoteAddr(), streamErr)
			}
		}(stream)
	}
}

// closeWrite signals the write-half of a connection is done, allowing the remote
// to flush any remaining data before the connection is fully torn down.
func closeWrite(c net.Conn) {
	type halfCloser interface{ CloseWrite() error }
	if hc, ok := c.(halfCloser); ok {
		hc.CloseWrite()
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
func handleTunnel(conn net.Conn, shaper *sessionShaper) error {
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
	target := net.JoinHostPort(host, fmt.Sprintf("%d", binary.BigEndian.Uint16(portBuf)))

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

	// Bidirectional copy with half-close so each side can flush remaining data.
	done := make(chan struct{}, 2)
	go func() {
		b := copyBufPool.Get().(*[]byte)
		io.CopyBuffer(remote, conn, *b)
		copyBufPool.Put(b)
		closeWrite(remote)
		done <- struct{}{}
	}()
	go func() {
		b := copyBufPool.Get().(*[]byte)
		var dst io.Writer = conn
		if shaper != nil {
			dst = shaper.downWriter(conn)
		}
		io.CopyBuffer(dst, remote, *b)
		copyBufPool.Put(b)
		closeWrite(conn)
		done <- struct{}{}
	}()
	<-done
	<-done

	return nil
}

// handleVisionTunnel handles a Vision TCP tunnel stream (cmd=0x03).
// Address parsing is identical to handleTunnel. After dialing the target,
// upload direction (client→remote) is Vision-decoded (strip padding/sentinel),
// download direction (remote→client) is Vision-encoded (add padding/sentinel).
// SmartShaper ↓ throttle is applied on the download path via shaper.downWriter.
func handleVisionTunnel(conn net.Conn, shaper *sessionShaper) error {
	atyp := make([]byte, 1)
	if _, err := io.ReadFull(conn, atyp); err != nil {
		return fmt.Errorf("vision read atyp: %w", err)
	}

	var host string
	switch atyp[0] {
	case 0x01:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return fmt.Errorf("vision read IPv4: %w", err)
		}
		host = net.IP(ip).String()
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return fmt.Errorf("vision read domain length: %w", err)
		}
		domain := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(conn, domain); err != nil {
			return fmt.Errorf("vision read domain: %w", err)
		}
		host = string(domain)
	case 0x04:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return fmt.Errorf("vision read IPv6: %w", err)
		}
		host = net.IP(ip).String()
	default:
		conn.Write([]byte{0x01})
		return fmt.Errorf("vision unsupported atyp: 0x%02x", atyp[0])
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return fmt.Errorf("vision read port: %w", err)
	}
	target := net.JoinHostPort(host, fmt.Sprintf("%d", binary.BigEndian.Uint16(portBuf)))

	remote, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		conn.Write([]byte{0x01})
		return fmt.Errorf("vision dial %s: %w", target, err)
	}
	defer remote.Close()

	if _, err := conn.Write([]byte{0x00}); err != nil {
		return fmt.Errorf("vision write ok: %w", err)
	}

	log.Printf("Vision proxying %s → %s", conn.RemoteAddr(), target)

	done := make(chan struct{}, 2)

	// Upload: client→remote — strip Vision framing, forward raw TLS to target.
	go func() {
		visionCopyFromTunnel(remote, conn)
		closeWrite(remote)
		done <- struct{}{}
	}()

	// Download: remote→client — add Vision framing, optional SmartShaper ↓ throttle.
	go func() {
		var dst io.Writer = conn
		if shaper != nil {
			dst = shaper.downWriter(conn)
		}
		visionCopyToTunnel(dst, remote)
		closeWrite(conn)
		done <- struct{}{}
	}()

	<-done
	<-done
	return nil
}

// handleUDPRelay handles a UDP relay stream from a client.
// It receives length-prefixed frames [4B len][ATYP+ADDR+PORT+DATA], dispatches
// them as UDP datagrams to the target, and returns responses in the same framing.
func handleUDPRelay(stream net.Conn) error {
	if _, err := stream.Write([]byte{0x00}); err != nil {
		return fmt.Errorf("write UDP ack: %w", err)
	}

	pc, err := net.ListenPacket("udp", "0.0.0.0:0")
	if err != nil {
		return fmt.Errorf("UDP listen: %w", err)
	}
	defer pc.Close()

	const idleTimeout = 2 * time.Minute

	// Stream → UDP: parse length-prefixed frames and dispatch to target.
	go func() {
		lenBuf := make([]byte, 4)
		addrCache := make(map[string]*net.UDPAddr)
		payloadBuf := make([]byte, 65535) // persistent read buffer; reused each iteration
		for {
			if _, err := io.ReadFull(stream, lenBuf); err != nil {
				pc.Close()
				return
			}
			payloadLen := binary.BigEndian.Uint32(lenBuf)
			if payloadLen > 65535 {
				pc.Close()
				return
			}
			payload := payloadBuf[:payloadLen] // reuse persistent buffer
			if _, err := io.ReadFull(stream, payload); err != nil {
				pc.Close()
				return
			}
			if len(payload) < 4 {
				continue
			}
			var host string
			var addrEnd int
			switch payload[0] {
			case 0x01: // IPv4
				if len(payload) < 7 {
					continue
				}
				host = net.IP(payload[1:5]).String()
				addrEnd = 5
			case 0x03: // domain
				if len(payload) < 3 {
					continue
				}
				nameLen := int(payload[1])
				if len(payload) < 2+nameLen+2 {
					continue
				}
				host = string(payload[2 : 2+nameLen])
				addrEnd = 2 + nameLen
			case 0x04: // IPv6
				if len(payload) < 19 {
					continue
				}
				host = net.IP(payload[1:17]).String()
				addrEnd = 17
			default:
				continue
			}
			if len(payload) < addrEnd+2 {
				continue
			}
			port := binary.BigEndian.Uint16(payload[addrEnd : addrEnd+2])
			data := payload[addrEnd+2:]
			target := net.JoinHostPort(host, fmt.Sprintf("%d", port))
			addr, ok := addrCache[target]
			if !ok {
				var err error
				addr, err = net.ResolveUDPAddr("udp", target)
				if err != nil {
					log.Printf("UDP relay: resolve %s: %v", target, err)
					continue
				}
				addrCache[target] = addr
			}
			pc.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := pc.WriteTo(data, addr); err != nil {
				log.Printf("UDP relay: write to %s: %v", target, err)
			}
		}
	}()

	// UDP → Stream: receive responses and forward to client as length-prefixed frames.
	buf := make([]byte, 65535)
	frameBuf := make([]byte, 4+1+16+2+65535) // persistent: [4B len][ATYP+IP+PORT][data]
	for {
		pc.SetReadDeadline(time.Now().Add(idleTimeout))
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return nil // idle timeout or PacketConn closed
		}
		udpAddr := addr.(*net.UDPAddr)
		// Build frame directly into persistent buffer: [4B len][ATYP+IP+PORT][data]
		off := 4 // reserve first 4 bytes for length prefix
		if ip4 := udpAddr.IP.To4(); ip4 != nil {
			frameBuf[off] = 0x01
			copy(frameBuf[off+1:], ip4)
			off += 5
		} else {
			frameBuf[off] = 0x04
			copy(frameBuf[off+1:], udpAddr.IP.To16())
			off += 17
		}
		frameBuf[off] = byte(udpAddr.Port >> 8)
		frameBuf[off+1] = byte(udpAddr.Port)
		off += 2
		copy(frameBuf[off:], buf[:n])
		off += n
		binary.BigEndian.PutUint32(frameBuf[:4], uint32(off-4))

		stream.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := stream.Write(frameBuf[:off]); err != nil {
			return fmt.Errorf("write UDP frame: %w", err)
		}
	}
}
