package torrent

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/big"
	mrand "math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/utp"
	"github.com/hashicorp/yamux"
	"github.com/miekg/dns"

	"umbrella_client/internal/client/config"
	"umbrella_client/internal/client/shaper"
	"umbrella_client/internal/client/share"
	"umbrella_client/internal/client/umbrella_dns"
	"umbrella_client/internal/storage"
)

const (
	pstr         = "BitTorrent protocol"
	pstrlen      = 19
	handshakeLen = 68
	pieceMsgID   = 7
	unchokeMsgID = 1
	interestedID = 2
)

var (
	cfg                 *config.Config
	gServerAddr         string
	gAuthKey            []byte
	gInfoHash           [20]byte
	gBypass             []string
	gDNSListen          string
	gDNSUpstream        string
	gShaper             bool
	gSessionsNum        int
	gConnectionsTimeOut time.Duration
	wg                  sync.WaitGroup

	// Session pool
	sessions     []*yamux.Session
	sessMu       []sync.Mutex
	getSessionMu sync.Mutex
)

// Start initializes and runs the Torrent client module.
func Start(c *config.Config, ctx context.Context, appFilesDir string, dnsCache *storage.DnsCache) error {
	// Start pprof server for memory profiling
	go func() {
		log.Println("[INFO] Starting pprof server on localhost:6060")
		if err := http.ListenAndServe("localhost:6060", nil); err != nil {
			log.Printf("[ERR] pprof server: %v", err)
		}
	}()

	cfg = c
	gServerAddr = cfg.Server
	gDNSListen = cfg.DNSListen
	gDNSUpstream = cfg.DNSUpstream
	gShaper = cfg.Shaper
	gConnectionsTimeOut = time.Duration(cfg.Torrent.ConnectionsTimeOut) * time.Second
	gBypass = make([]string, len(cfg.Bypass))
	copy(gBypass, cfg.Bypass)

	gSessionsNum = cfg.Torrent.SessionsNum
	if gSessionsNum < 1 {
		gSessionsNum = 5
	}
	sessions = make([]*yamux.Session, gSessionsNum)
	sessMu = make([]sync.Mutex, gSessionsNum)

	var err error
	if cfg.Torrent.AuthKey == "" {
		return fmt.Errorf("auth-key is required for torrent protocol")
	}
	gAuthKey, err = hex.DecodeString(cfg.Torrent.AuthKey)
	if err != nil || len(gAuthKey) != 32 {
		return fmt.Errorf("invalid auth-key: must be 64 hex chars (32 bytes)")
	}

	// Prepare InfoHash
	if cfg.Torrent.InfoHash != "" {
		ih, err := hex.DecodeString(cfg.Torrent.InfoHash)
		if err != nil || len(ih) != 20 {
			return fmt.Errorf("invalid info-hash: must be 40 hex chars (20 bytes)")
		}
		copy(gInfoHash[:], ih)
	} else {
		// Generate random InfoHash if not provided
		rand.Read(gInfoHash[:])
		log.Printf("[INFO] Using random info-hash: %s", hex.EncodeToString(gInfoHash[:]))
	}

	// Standard bypasses
	serverHost, _, err := net.SplitHostPort(gServerAddr)
	if err != nil {
		serverHost = gServerAddr
	}
	gBypass = append(gBypass, serverHost)
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

	// Start SOCKS5 listener
	go listenSOCKS5(ctx, dnsCache)

	// Start White Noise generator (Trackers Announce)
	go startWhiteNoise(ctx)

	if gDNSListen != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			umbrella_dns.RunDNSServer(ctx, dnsCache, gBypass, gDNSListen, gDNSUpstream, forwardDNS)
		}()
	}

	log.Printf("[INFO] Umbrella/Torrent client (uTP) on %s → %s", cfg.ListenAddr, cfg.Server)

	<-ctx.Done()
	log.Printf("[INFO] Umbrella/Torrent client listener closed, stopping")
	return nil
}

func listenSOCKS5(ctx context.Context, dnsCache *storage.DnsCache) {
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Printf("[ERR] SOCKS5 listen: %v", err)
		return
	}
	defer ln.Close()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("[ERR] SOCKS5 accept: %v", err)
			continue
		}
		wg.Add(1)
		go handleSOCKS5(ctx, conn, dnsCache)
	}
}

func handleSOCKS5(ctx context.Context, conn net.Conn, dnsCache *storage.DnsCache) {
	defer wg.Done()

	sCtx, sCancel := context.WithCancel(ctx)
	defer sCancel()
	defer conn.Close()

	conn = &timeoutConn{Conn: conn}

	cmd, host, port, err := share.Socks5Handshake(conn, cfg.UDPEnabled)
	if err != nil {
		log.Printf("[ERR] SOCKS5 handshake error from %s: %v", conn.RemoteAddr(), err)
		return
	}

	if cmd == 0x03 {
		handleSocks5UDP(ctx, conn, dnsCache)
		return
	}

	// Bypass logic
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
		conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		share.HandleDirect(sCtx, conn, host, port)
		return
	}

	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	target := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	log.Printf("[INFO] Tunneling %s → %s", conn.RemoteAddr(), target)

	var rawStream net.Conn
	var sErr error

	for attempt := 0; attempt < 3; attempt++ {
		sess, err := getSession(sCtx)
		if err != nil {
			log.Printf("[ERR] failed to get session: %v", err)
			return
		}

		rawStream, sErr = sess.Open()
		if sErr == nil {
			targetBytes := []byte(target)
			lenBuf := make([]byte, 4)
			binary.BigEndian.PutUint32(lenBuf, uint32(len(targetBytes)))
			if _, err := rawStream.Write(lenBuf); err == nil {
				if _, err := rawStream.Write(targetBytes); err == nil {
					break
				}
			}
			rawStream.Close()
			dropSession(sess)
		}
	}

	if sErr != nil || rawStream == nil {
		return
	}

	stream := &timeoutConn{Conn: rawStream}
	defer stream.Close()

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

	<-sCtx.Done()
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

func handleSocks5UDP(ctx context.Context, rawTcpConn net.Conn, dnsCache *storage.DnsCache) {
	// Create local UDP socket for SOCKS5 app
	rawUdpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		rawTcpConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		log.Printf("[ERR] UDP ASSOCIATE listen error: %v", err)
		return
	}
	udpConn := &timeoutPacketConn{PacketConn: rawUdpConn}

	// Жизненный цикл UDP релея
	udpCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer udpConn.Close()

	udpAddr := udpConn.LocalAddr().(*net.UDPAddr)
	reply := []byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, byte(udpAddr.Port >> 8), byte(udpAddr.Port)}
	if _, err := rawTcpConn.Write(reply); err != nil {
		return
	}

	var addrMu sync.Mutex
	var appAddr net.Addr

	// Get a session and open a stream for UDP relay
	var rawStream net.Conn
	for attempt := 0; attempt < 3; attempt++ {
		sess, err := getSession(udpCtx)
		if err != nil {
			log.Printf("[ERR] failed to get session for UDP: %v", err)
			return
		}

		s, err := sess.Open()
		if err == nil {
			targetBytes := []byte("UDP_RELAY")
			lenBuf := make([]byte, 4)
			binary.BigEndian.PutUint32(lenBuf, uint32(len(targetBytes)))
			if _, err := s.Write(lenBuf); err == nil {
				if _, err := s.Write(targetBytes); err == nil {
					rawStream = s
					break
				}
			}
			s.Close()
			dropSession(sess)
		}
	}

	if rawStream == nil {
		log.Printf("[ERR] failed to establish UDP stream after retries")
		return
	}

	stream := &timeoutConn{Conn: rawStream}
	defer stream.Close()

	var relayWriter io.Writer = stream
	var relayReader io.Reader = stream
	if gShaper {
		relayWriter = &shaper.ShapedWriter{W: stream, Bucket: &shaper.GUpBucket}
		relayReader = &shaper.ShapedReader{R: stream, Bucket: &shaper.GDownBucket}
	}

	log.Printf("[INFO] UDP relay active, local UDP: %s", udpAddr)

	// App -> Server / Bypass
	go func() {
		defer cancel()
		bufPtr := share.UDPBufPool.Get().(*[]byte)
		defer share.UDPBufPool.Put(bufPtr)
		buf := *bufPtr

		frameBufPtr := share.UDPBufPool.Get().(*[]byte)
		defer share.UDPBufPool.Put(frameBufPtr)
		frameBuf := *frameBufPtr

		for {
			n, addr, err := udpConn.ReadFrom(buf)
			if err != nil {
				return
			}

			addrMu.Lock()
			if appAddr == nil {
				appAddr = addr
			}
			isApp := addr.String() == appAddr.String()
			addrMu.Unlock()

			if !isApp {
				addrMu.Lock()
				app := appAddr
				addrMu.Unlock()
				if app != nil {
					udpSrc := addr.(*net.UDPAddr)
					resp := []byte{0, 0, 0}
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

			if n < 4 || buf[2] != 0x00 {
				continue
			}

			host, port, dataStart, err := parseSOCKS5UDPAddr(buf)
			if err != nil {
				continue
			}

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
				targetAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
				if err == nil {
					udpConn.WriteTo(buf[dataStart:n], targetAddr)
				}
				continue
			}

			// Tunnel
			payload := buf[3:n]
			binary.BigEndian.PutUint32(frameBuf[:4], uint32(len(payload)))
			copy(frameBuf[4:], payload)
			if _, err := relayWriter.Write(frameBuf[:4+len(payload)]); err != nil {
				return
			}
		}
	}()

	// Server -> App
	go func() {
		defer cancel()
		lenBuf := make([]byte, 4)
		pktBufPtr := share.UDPBufPool.Get().(*[]byte)
		defer share.UDPBufPool.Put(pktBufPtr)
		pktBuf := *pktBufPtr
		pktBuf[0] = 0x00
		pktBuf[1] = 0x00
		pktBuf[2] = 0x00
		for {
			if _, err := io.ReadFull(relayReader, lenBuf); err != nil {
				return
			}

			payloadLen := binary.BigEndian.Uint32(lenBuf)
			if payloadLen > 65535 {
				return
			}
			payload := pktBuf[3 : 3+payloadLen]
			if _, err := io.ReadFull(relayReader, payload); err != nil {
				return
			}
			addrMu.Lock()
			a := appAddr
			addrMu.Unlock()
			if a != nil {
				udpConn.WriteTo(pktBuf[:3+payloadLen], a)
			}
		}
	}()

	// Ждем закрытия контекста или TCP соединения
	<-udpCtx.Done()
}

func parseSOCKS5UDPAddr(buf []byte) (host string, port uint16, dataStart int, err error) {
	if len(buf) < 4 {
		return "", 0, 0, fmt.Errorf("packet too short")
	}
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
		return "", 0, 0, fmt.Errorf("unknown address type %d", atyp)
	}
	return host, port, dataStart, nil
}

func getSession(ctx context.Context) (*yamux.Session, error) {
	getSessionMu.Lock()
	defer getSessionMu.Unlock()

	idx := mrand.Intn(gSessionsNum)
	if sessions[idx] != nil && !sessions[idx].IsClosed() {
		return sessions[idx], nil
	}

	sess, err := establishSession(ctx, idx)
	if err == nil {
		return sess, nil
	}

	for i := 0; i < gSessionsNum; i++ {
		if sessions[i] != nil && !sessions[i].IsClosed() {
			return sessions[i], nil
		}
	}

	return nil, fmt.Errorf("no available sessions: %w", err)
}

func establishSession(ctx context.Context, idx int) (*yamux.Session, error) {
	sessMu[idx].Lock()
	defer sessMu[idx].Unlock()

	if sessions[idx] != nil && !sessions[idx].IsClosed() {
		return sessions[idx], nil
	}

	// Resolve server address and handle port hopping
	host, portStr, err := net.SplitHostPort(gServerAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid server address: %w", err)
	}

	var targetAddr string
	if strings.Contains(portStr, "-") {
		parts := strings.Split(portStr, "-")
		startPort, _ := strconv.Atoi(parts[0])
		endPort, _ := strconv.Atoi(parts[1])
		randomPort := startPort + mrand.Intn(endPort-startPort+1)
		targetAddr = net.JoinHostPort(host, strconv.Itoa(randomPort))
	} else {
		targetAddr = gServerAddr
	}

	// 1. Create uTP connection
	s, err := utp.Dial(targetAddr)
	if err != nil {
		return nil, err
	}

	// 2. Send BitTorrent Handshake with HMAC
	peerID := generatePeerID()
	handshake := make([]byte, handshakeLen)
	handshake[0] = pstrlen
	copy(handshake[1:20], pstr)
	copy(handshake[28:48], gInfoHash[:])
	copy(handshake[48:68], peerID[:])

	if _, err := s.Write(handshake); err != nil {
		s.Close()
		return nil, err
	}

	// 3. Receive server handshake
	s.SetReadDeadline(time.Now().Add(30 * time.Second))
	respHandshake := make([]byte, handshakeLen)
	if _, err := io.ReadFull(s, respHandshake); err != nil {
		s.Close()
		return nil, err
	}
	s.SetReadDeadline(time.Time{}) // Reset deadline

	// 4. Send "Interested" and "Unchoke"
	s.Write([]byte{0, 0, 0, 1, interestedID})
	s.Write([]byte{0, 0, 0, 1, unchokeMsgID})

	// 5. Setup yamux
	muxCfg := yamux.DefaultConfig()
	muxCfg.MaxStreamWindowSize = 8 * 1024 * 1024
	muxCfg.EnableKeepAlive = true
	muxCfg.StreamCloseTimeout = 10 * time.Second
	muxCfg.LogOutput = io.Discard

	sess, err := yamux.Client(&TorrentConn{Conn: s}, muxCfg)
	if err != nil {
		s.Close()
		return nil, err
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		scheduleReconnect(ctx, sess)
	}()

	sessions[idx] = sess
	return sess, nil
}

func scheduleReconnect(ctx context.Context, s *yamux.Session) {
	// Random delay in [3, 15) minutes using crypto/rand for unpredictability.
	const minDelay = 3 * time.Minute
	const maxJitter = 12 * time.Minute

	delay := minDelay
	n, err := rand.Int(rand.Reader, big.NewInt(int64(maxJitter)))
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

func dropSession(s *yamux.Session) {
	for i := 0; i < gSessionsNum; i++ {
		sessMu[i].Lock()
		if sessions[i] == s {
			sessions[i] = nil
			go s.Close()
			sessMu[i].Unlock()
			break
		}
		sessMu[i].Unlock()
	}
}

func forwardDNS(ctx context.Context, r *dns.Msg) (*dns.Msg, error) {
	dnsData, err := r.Pack()
	if err != nil {
		return nil, fmt.Errorf("pack dns: %w", err)
	}

	host, portStr, _ := net.SplitHostPort(gDNSUpstream)
	var port uint16
	fmt.Sscanf(portStr, "%d", &port)

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

	// Get a session and open a stream for DNS
	var respPayload []byte
	for attempt := 0; attempt < 3; attempt++ {
		sess, err := getSession(ctx)
		if err != nil {
			return nil, err
		}

		stream, err := sess.Open()
		if err != nil {
			dropSession(sess)
			continue
		}

		stream = &timeoutConn{Conn: stream}

		// Use a helper function to ensure stream is closed properly in each attempt
		respPayload, err = func() ([]byte, error) {
			defer stream.Close()

			// Send special "DNS_QUERY" target
			targetBytes := []byte("DNS_QUERY")
			lenBuf := make([]byte, 4)
			binary.BigEndian.PutUint32(lenBuf, uint32(len(targetBytes)))
			if _, err := stream.Write(lenBuf); err != nil {
				return nil, err
			}
			if _, err := stream.Write(targetBytes); err != nil {
				return nil, err
			}

			var relayWriter io.Writer = stream
			var relayReader io.Reader = stream
			if gShaper {
				relayWriter = &shaper.ShapedWriter{W: stream, Bucket: &shaper.GUpBucket}
				relayReader = &shaper.ShapedReader{R: stream, Bucket: &shaper.GDownBucket}
			}

			// Send DNS query
			binary.BigEndian.PutUint32(lenBuf, uint32(len(payload)))
			if _, err := relayWriter.Write(lenBuf); err != nil {
				return nil, err
			}
			if _, err := relayWriter.Write(payload); err != nil {
				return nil, err
			}

			// Read DNS response
			if _, err := io.ReadFull(relayReader, lenBuf); err != nil {
				return nil, err
			}
			respLen := binary.BigEndian.Uint32(lenBuf)
			if respLen > 65535 {
				return nil, fmt.Errorf("dns response too large: %d", respLen)
			}
			data := make([]byte, respLen)
			if _, err := io.ReadFull(relayReader, data); err != nil {
				return nil, err
			}
			return data, nil
		}()

		if err == nil {
			break
		}
		log.Printf("[DEBUG] DNS attempt %d failed: %v", attempt+1, err)
	}

	if respPayload == nil {
		return nil, fmt.Errorf("failed to forward DNS after retries")
	}

	off := 0
	if len(respPayload) > 0 {
		switch respPayload[0] {
		case 0x01:
			off = 1 + 4 + 2
		case 0x04:
			off = 1 + 16 + 2
		case 0x03:
			if len(respPayload) > 1 {
				off = 1 + 1 + int(respPayload[1]) + 2
			}
		}
	}

	msg := new(dns.Msg)
	if err := msg.Unpack(respPayload[off:]); err != nil {
		return nil, fmt.Errorf("unpack dns response: %w", err)
	}

	return msg, nil
}

func generatePeerID() [20]byte {
	var pid [20]byte
	copy(pid[:8], []byte("-TR4000-"))
	nonce := make([]byte, 4)
	rand.Read(nonce)
	copy(pid[8:12], nonce)
	mac := hmac.New(sha1.New, gAuthKey)
	mac.Write(nonce)
	mac.Write(gInfoHash[:])
	sig := mac.Sum(nil)[:8]
	copy(pid[12:20], sig)
	return pid
}

type TorrentConn struct {
	net.Conn
	reader           *bufio.Reader
	remainingPayload int
	remainingPadding int
}

func (c *TorrentConn) Read(p []byte) (n int, err error) {
	if c.reader == nil {
		c.reader = bufio.NewReaderSize(c.Conn, 256*1024)
	}

	for {
		if c.remainingPayload > 0 {
			toRead := c.remainingPayload
			if toRead > len(p) {
				toRead = len(p)
			}
			n, err = c.reader.Read(p[:toRead])
			if err != nil {
				return n, err
			}
			c.remainingPayload -= n
			return n, nil
		}

		// Если полезная нагрузка считана, но остался паддинг — сбрасываем его
		if c.remainingPadding > 0 {
			if _, err := c.reader.Discard(c.remainingPadding); err != nil {
				return 0, err
			}
			c.remainingPadding = 0
		}

		var header [4]byte
		// Читаем длину (4 байта)
		if _, err := io.ReadFull(c.reader, header[:]); err != nil {
			return 0, err
		}
		length := binary.BigEndian.Uint32(header[:])
		if length == 0 {
			continue
		}

		// Читаем ID (1 байт)
		msgID, err := c.reader.ReadByte()
		if err != nil {
			return 0, err
		}

		if msgID == pieceMsgID {
			// Discard Index(4) + Offset(4) = 8 bytes
			if _, err := c.reader.Discard(8); err != nil {
				return 0, err
			}

			// Читаем наш внутренний заголовок PayloadLen (2 байта)
			var pLenBuf [2]byte
			if _, err := io.ReadFull(c.reader, pLenBuf[:]); err != nil {
				return 0, err
			}
			pLen := int(binary.BigEndian.Uint16(pLenBuf[:]))

			// Общий остаток в этом piece (за вычетом Index+Offset+PayloadLen)
			totalInPiece := int(length) - 11
			if pLen > totalInPiece {
				return 0, fmt.Errorf("torrent protocol corruption: payload %d > total %d", pLen, totalInPiece)
			}

			c.remainingPayload = pLen
			c.remainingPadding = totalInPiece - pLen

			if c.remainingPayload == 0 {
				// Если пакет пустой (только паддинг), продолжаем цикл
				continue
			}
		} else {
			// Пропускаем другие типы сообщений
			if _, err := c.reader.Discard(int(length) - 1); err != nil {
				return 0, err
			}
		}
	}
}

func (c *TorrentConn) Write(p []byte) (n int, err error) {
	const bittorrentHeadLen = 4 + 1 + 4 + 4 // Length + ID + Index + Offset
	const internalHeadLen = 2               // PayloadLen

	// Выбираем рандомный размер паддинга (0..255 байт)
	padLen := mrand.Intn(256)

	bufPtr := share.UDPBufPool.Get().(*[]byte)
	defer share.UDPBufPool.Put(bufPtr)
	buf := *bufPtr

	totalMsgLen := 9 + internalHeadLen + len(p) + padLen
	binary.BigEndian.PutUint32(buf[0:4], uint32(totalMsgLen))
	buf[4] = pieceMsgID
	binary.BigEndian.PutUint32(buf[5:9], 0)  // Index
	binary.BigEndian.PutUint32(buf[9:13], 0) // Offset

	// Наш внутренний заголовок
	binary.BigEndian.PutUint16(buf[13:15], uint16(len(p)))

	// Данные
	copy(buf[15:], p)

	// Паддинг (используем то что уже было в буфере для скорости или забиваем мусором)
	if padLen > 0 {
		// Для лучшей энтропии можно забить случайными данными,
		// но на практике мусор из пула буферов тоже неплох.
		// Забьем немного реального рандома для надежности.
		rand.Read(buf[15+len(p) : 15+len(p)+padLen])
	}

	_, err = c.Conn.Write(buf[:4+totalMsgLen])
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func startWhiteNoise(ctx context.Context) {
	trackers := []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"udp://tracker.openbittorrent.com:6969/announce",
		"udp://9.rarbg.com:2810/announce",
		"udp://exodus.desync.com:6969/announce",
		"udp://open.stealth.si:80/announce",
	}

	peerID := generatePeerID()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	// Сразу сделаем первый анонс
	go announceToAll(trackers, gInfoHash, peerID)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			go announceToAll(trackers, gInfoHash, peerID)
		}
	}
}

func announceToAll(trackers []string, infoHash, peerID [20]byte) {
	for _, tr := range trackers {
		go func(urlStr string) {
			if err := announceToTracker(urlStr, infoHash, peerID); err != nil {
				// log.Printf("[DEBUG] WhiteNoise: announce to %s failed: %v", urlStr, err)
			} else {
				log.Printf("[INFO] WhiteNoise: Announced to tracker %s", urlStr)
			}
		}(tr)
	}
}

func announceToTracker(urlStr string, infoHash, peerID [20]byte) error {
	u, err := net.ResolveUDPAddr("udp", strings.TrimPrefix(urlStr, "udp://"))
	if err != nil {
		return err
	}

	conn, err := net.DialUDP("udp", nil, u)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	// 1. Connection request
	transactionID := uint32(mrand.Int31())
	req := make([]byte, 16)
	binary.BigEndian.PutUint64(req[0:8], 0x41727101980) // protocol_id
	binary.BigEndian.PutUint32(req[8:12], 0)            // action: connect
	binary.BigEndian.PutUint32(req[12:16], transactionID)

	if _, err := conn.Write(req); err != nil {
		return err
	}

	resp := make([]byte, 16)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return err
	}

	if binary.BigEndian.Uint32(resp[0:4]) != 0 || binary.BigEndian.Uint32(resp[4:8]) != transactionID {
		return fmt.Errorf("invalid connect response")
	}
	connectionID := binary.BigEndian.Uint64(resp[8:16])

	// 2. Announce request
	ann := make([]byte, 98)
	binary.BigEndian.PutUint64(ann[0:8], connectionID)
	binary.BigEndian.PutUint32(ann[8:12], 1) // action: announce
	binary.BigEndian.PutUint32(ann[12:16], transactionID)
	copy(ann[16:36], infoHash[:])
	copy(ann[36:56], peerID[:])
	binary.BigEndian.PutUint64(ann[56:64], 0)          // downloaded
	binary.BigEndian.PutUint64(ann[64:72], 0)          // left (fake)
	binary.BigEndian.PutUint64(ann[72:80], 0)          // uploaded
	binary.BigEndian.PutUint32(ann[80:84], 0)          // event: none
	binary.BigEndian.PutUint32(ann[84:88], 0)          // IP
	binary.BigEndian.PutUint32(ann[88:92], 0)          // key
	binary.BigEndian.PutUint32(ann[92:96], 0xFFFFFFFF) // num_want: -1 means default
	binary.BigEndian.PutUint16(ann[96:98], 6881)       // port

	if _, err := conn.Write(ann); err != nil {
		return err
	}
	return nil
}
