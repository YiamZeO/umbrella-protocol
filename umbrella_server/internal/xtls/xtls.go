package xtls

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"umbrella_server/internal/config"

	"github.com/hashicorp/yamux"
	"github.com/xtls/reality"
)

var copyBufPool = sync.Pool{New: func() any { b := make([]byte, 32*1024); return &b }}

func XtlsStarter(cfg *config.Config) {
	privKeyBytes := make([]byte, 32)
	if cfg.Xtls.PrivateKey == "" {
		if _, err := rand.Read(privKeyBytes); err != nil {
			log.Fatalf("[ERR] generate private key: %v", err)
		}
		log.Printf("[INFO] Generated private key — save this for private-key on restart: %s", base64.StdEncoding.EncodeToString(privKeyBytes))
	} else {
		b, err := base64.StdEncoding.DecodeString(cfg.Xtls.PrivateKey)
		if err != nil || len(b) != 32 {
			log.Fatalf("[ERR] private-key must be base64 of exactly 32 bytes")
		}
		privKeyBytes = b
	}

	privKey, err := ecdh.X25519().NewPrivateKey(privKeyBytes)
	if err != nil {
		log.Fatalf("[ERR] invalid x25519 private key: %v", err)
	}
	pubKeyBytes := privKey.PublicKey().Bytes()
	log.Printf("[INFO] Public key (use as client --public-key): %s", base64.StdEncoding.EncodeToString(pubKeyBytes))

	var shortIdArr [8]byte
	if cfg.Xtls.ShortId == "" {
		if _, err := rand.Read(shortIdArr[:]); err != nil {
			log.Fatalf("[ERR] generate short ID: %v", err)
		}
		log.Printf("[INFO] Short ID (use as client --short-id): %s", hex.EncodeToString(shortIdArr[:]))
	} else {
		b, err := hex.DecodeString(cfg.Xtls.ShortId)
		if err != nil || len(b) > 8 {
			log.Fatalf("[ERR] short-id must be up to 16 hex chars")
		}
		copy(shortIdArr[:], b)
	}

	dest := cfg.Dest
	if _, _, err := net.SplitHostPort(dest); err != nil {
		dest = dest + ":443"
	}
	serverNames := map[string]bool{}
	if cfg.Xtls.ServerNames != "" {
		for _, name := range strings.Split(cfg.Xtls.ServerNames, ",") {
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
	log.Printf("[INFO] Allowed SNIs: %v", serverNames)

	realityConf := &reality.Config{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, network, address)
		},
		Type:                   "tcp",
		Dest:                   dest,
		ServerNames:            serverNames,
		PrivateKey:             privKeyBytes,
		ShortIds:               map[[8]byte]bool{shortIdArr: true},
		Show:                   cfg.Debug,
		SessionTicketsDisabled: true,
	}

	ln, err := net.Listen("tcp", ":"+cfg.Port)
	if err != nil {
		log.Fatalf("[ERR] listen :%s: %v", cfg.Port, err)
	}
	log.Printf("[INFO] Umbrella/Reality server on :%s (fallback → %s)", cfg.Port, dest)

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

func handleConn(conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[Panic] xtls handleConn from %s: %v", conn.RemoteAddr(), r)
		}
		conn.Close()
	}()
	muxCfg := yamux.DefaultConfig()
	muxCfg.MaxStreamWindowSize = 8 * 1024 * 1024
	muxCfg.EnableKeepAlive = true
	muxCfg.StreamCloseTimeout = 10 * time.Second
	muxCfg.LogOutput = io.Discard
	muxSess, err := yamux.Server(conn, muxCfg)
	if err != nil {
		log.Printf("[ERR] yamux server %s: %v", conn.RemoteAddr(), err)
		return
	}
	defer muxSess.Close()

	log.Printf("Session from %s", conn.RemoteAddr())
	for {
		stream, err := muxSess.Accept()
		if err != nil {
			log.Printf("[INFO] Session %s closed: %v", conn.RemoteAddr(), err)
			break
		}
		go func(s net.Conn) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[Panic] xtls handleStream from %s: %v", conn.RemoteAddr(), r)
				}
				s.Close()
			}()
			cmdBuf := make([]byte, 1)
			if _, err := io.ReadFull(s, cmdBuf); err != nil {
				return
			}
			var streamErr error
			switch cmdBuf[0] {
			case 0x00:
				streamErr = handleTunnel(s)
			case 0x01:
				streamErr = handleUDPRelay(s)
			case 0x03:
				streamErr = handleVisionTunnel(s)
			default:
				log.Printf("[ERR] unknown stream cmd 0x%02x from %s", cmdBuf[0], conn.RemoteAddr())
				return
			}
			if streamErr != nil && !strings.Contains(streamErr.Error(), "EOF") && !strings.Contains(streamErr.Error(), "closed") {
				log.Printf("[ERR] stream error from %s: %v", conn.RemoteAddr(), streamErr)
			}
		}(stream)
	}
}

func closeWrite(c net.Conn) {
	type halfCloser interface{ CloseWrite() error }
	if hc, ok := c.(halfCloser); ok {
		hc.CloseWrite()
	}
}

func handleTunnel(conn net.Conn) error {
	atyp := make([]byte, 1)
	if _, err := io.ReadFull(conn, atyp); err != nil {
		return fmt.Errorf("read atyp: %w", err)
	}

	var host string
	switch atyp[0] {
	case 0x01:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return fmt.Errorf("read IPv4: %w", err)
		}
		host = net.IP(ip).String()
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return fmt.Errorf("read domain length: %w", err)
		}
		domain := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(conn, domain); err != nil {
			return fmt.Errorf("read domain: %w", err)
		}
		host = string(domain)
	case 0x04:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return fmt.Errorf("read IPv6: %w", err)
		}
		host = net.IP(ip).String()
	default:
		conn.Write([]byte{0x01})
		return fmt.Errorf("unsupported address type: 0x%02x", atyp[0])
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return fmt.Errorf("read port: %w", err)
	}
	target := net.JoinHostPort(host, fmt.Sprintf("%d", binary.BigEndian.Uint16(portBuf)))

	remote, err := net.DialTimeout("tcp", target, 30*time.Second)
	if err != nil {
		conn.Write([]byte{0x01})
		return fmt.Errorf("dial %s: %w", target, err)
	}
	defer remote.Close()

	if _, err := conn.Write([]byte{0x00}); err != nil {
		return fmt.Errorf("write ok: %w", err)
	}

	log.Printf("Proxying %s → %s", conn.RemoteAddr(), target)

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
		io.CopyBuffer(conn, remote, *b)
		copyBufPool.Put(b)
		closeWrite(conn)
		done <- struct{}{}
	}()
	<-done
	<-done

	return nil
}

func handleVisionTunnel(conn net.Conn) error {
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

	remote, err := net.DialTimeout("tcp", target, 30*time.Second)
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

	go func() {
		visionCopyFromTunnel(remote, conn)
		closeWrite(remote)
		done <- struct{}{}
	}()

	go func() {
		visionCopyToTunnel(conn, remote)
		closeWrite(conn)
		done <- struct{}{}
	}()

	<-done
	<-done
	return nil
}

func handleUDPRelay(stream net.Conn) error {
	if _, err := stream.Write([]byte{0x00}); err != nil {
		return fmt.Errorf("write UDP ack: %w", err)
	}

	pc, err := net.ListenPacket("udp", "0.0.0.0:0")
	if err != nil {
		return fmt.Errorf("UDP listen: %w", err)
	}
	defer pc.Close()

	const idleTimeout = 30 * time.Second

	go func() {
		lenBuf := make([]byte, 4)
		addrCache := make(map[string]*net.UDPAddr)
		payloadBuf := make([]byte, 65535)
		for {
			if _, err := io.ReadFull(stream, lenBuf); err != nil {
				go pc.Close()
				return
			}
			payloadLen := binary.BigEndian.Uint32(lenBuf)
			if payloadLen > 65535 {
				go pc.Close()
				return
			}
			payload := payloadBuf[:payloadLen]
			if _, err := io.ReadFull(stream, payload); err != nil {
				go pc.Close()
				return
			}
			if len(payload) < 4 {
				continue
			}
			var host string
			var addrEnd int
			switch payload[0] {
			case 0x01:
				if len(payload) < 7 {
					continue
				}
				host = net.IP(payload[1:5]).String()
				addrEnd = 5
			case 0x03:
				if len(payload) < 3 {
					continue
				}
				nameLen := int(payload[1])
				if len(payload) < 2+nameLen+2 {
					continue
				}
				host = string(payload[2 : 2+nameLen])
				addrEnd = 2 + nameLen
			case 0x04:
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
			pc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := pc.WriteTo(data, addr); err != nil {
				log.Printf("UDP relay: write to %s: %v", target, err)
			}
		}
	}()

	buf := make([]byte, 65535)
	frameBuf := make([]byte, 4+1+16+2+65535)
	for {
		pc.SetReadDeadline(time.Now().Add(idleTimeout))
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return nil
		}
		udpAddr := addr.(*net.UDPAddr)
		off := 4
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
		if _, err := stream.Write(frameBuf[:off]); err != nil {
			return fmt.Errorf("UDP relay stream write: %w", err)
		}
	}
}
