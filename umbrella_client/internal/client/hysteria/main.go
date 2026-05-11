package hysteria

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/quic-go"
	"github.com/miekg/dns"

	"umbrella_client/internal/client/config"
	"umbrella_client/internal/client/shaper"
	"umbrella_client/internal/client/share"
	"umbrella_client/internal/client/umbrella_dns"
	"umbrella_client/internal/storage"
)

type customCIDGenerator struct {
	authKey []byte
}

func (g *customCIDGenerator) GenerateConnectionID() (quic.ConnectionID, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return quic.ConnectionID{}, err
	}

	mac := hmac.New(sha512.New, g.authKey)
	mac.Write(nonce)
	signature := mac.Sum(nil)[:8]

	cid := make([]byte, 20)
	copy(cid[:12], nonce)
	copy(cid[12:], signature)

	return quic.ConnectionIDFromBytes(cid), nil
}

func (g *customCIDGenerator) ConnectionIDLen() int {
	return 20
}

var (
	cfg              *config.Config
	gServerAddr      string
	gDNSListen       string
	gDNSUpstream     string
	gAuthKey         []byte
	gListenAddr      string
	gBypass          []string
	gShaper          bool
	hyClientPoolSize int
	tcpSessions      []*tcpSession
	tcpSessionsMu    sync.Mutex
	clientPool       []client.Client
	clientPoolMu     sync.Mutex
	poolIdx          int
	wg               sync.WaitGroup
)

type tcpSession struct {
	tcpConn net.Conn
}

func Start(c *config.Config, ctx context.Context, appFilesDir string, dnsCache *storage.DnsCache) error {
	cfg = c
	gServerAddr = cfg.Server
	gListenAddr = cfg.ListenAddr
	gDNSListen = cfg.DNSListen
	gDNSUpstream = cfg.DNSUpstream
	hyClientPoolSize = cfg.Hysteria.ConnsNum
	gBypass = make([]string, len(cfg.Bypass))
	copy(gBypass, cfg.Bypass)

	var err error
	if cfg.Hysteria.AuthKey == "" {
		return fmt.Errorf("auth-key is required for hysteria protocol")
	}
	gAuthKey, err = hex.DecodeString(cfg.Hysteria.AuthKey)
	if err != nil || len(gAuthKey) != 32 {
		return fmt.Errorf("invalid auth-key: must be 64 hex chars (32 bytes)")
	}

	if host, _, err := net.SplitHostPort(gServerAddr); err == nil {
		gBypass = append(gBypass, host)
	} else {
		gBypass = append(gBypass, gServerAddr)
	}

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

	gShaper = cfg.Shaper
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

	go listenSOCKS5(ctx, dnsCache)

	if gDNSListen != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			umbrella_dns.RunDNSServer(ctx, dnsCache, gBypass, gDNSListen, gDNSUpstream, forwardDNS)
		}()
	}

	log.Printf("[INFO] Umbrella/Hysteria client on %s (SOCKS5) → %s (SNI: %s)", cfg.ListenAddr, cfg.Server, cfg.SNI)

	<-ctx.Done()

	log.Printf("[INFO] Umbrella/Hysteria client listener closed, stopping")

	tcpSessionsMu.Lock()
	for _, s := range tcpSessions {
		if s.tcpConn != nil {
			s.tcpConn.Close()
		}
	}
	tcpSessions = nil
	tcpSessionsMu.Unlock()

	clientPoolMu.Lock()
	for _, c := range clientPool {
		if c != nil {
			c.Close()
		}
	}
	clientPool = nil
	clientPoolMu.Unlock()

	return nil
}

func listenSOCKS5(ctx context.Context, dnsCache *storage.DnsCache) {
	ln, err := net.Listen("tcp", gListenAddr)
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
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			log.Printf("[ERR] SOCKS5 accept: %v", err)
			continue
		}
		wg.Add(1)
		go handleSOCKS5(ctx, conn, dnsCache)
	}
}

func handleSOCKS5(ctx context.Context, conn net.Conn, dnsCache *storage.DnsCache) {
	defer wg.Done()
	defer conn.Close()

	cmd, host, port, err := share.Socks5Handshake(conn, cfg.UDPEnabled)
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

	if cmd == 0x03 {
		if err := handleSocks5UDP(ctx, conn, dnsCache); err != nil {
			log.Printf("[ERR] SOCKS5 UDP ASSOCIATE failed: %v", err)
		}
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

	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})

	target := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	log.Printf("[INFO] Tunneling %s → %s", conn.RemoteAddr(), target)

	hyConn, err := dialTCP(target)
	if err != nil {
		log.Printf("[ERR] dial to %s → %v", target, err)
		return
	}

	// Add to session tracking before starting goroutines
	tcpSessionsMu.Lock()
	tcpSessions = append(tcpSessions, &tcpSession{tcpConn: hyConn})
	tcpSessionsMu.Unlock()

	// Ensure cleanup happens in both success and error cases
	defer func() {
		hyConn.Close()
		tcpSessionsMu.Lock()
		for i, s := range tcpSessions {
			if s.tcpConn == hyConn {
				tcpSessions = append(tcpSessions[:i], tcpSessions[i+1:]...)
				break
			}
		}
		tcpSessionsMu.Unlock()
	}()

	var uploadWriter io.Writer = hyConn
	var downloadReader io.Reader = hyConn
	if gShaper {
		uploadWriter = &shaper.ShapedWriter{W: hyConn, Bucket: &shaper.GUpBucket}
		downloadReader = &shaper.ShapedReader{R: hyConn, Bucket: &shaper.GDownBucket}
	}

	// Relay data with half-close support
	go func() {
		b := share.CopyBufPool.Get().(*[]byte)
		defer share.CopyBufPool.Put(b)
		io.CopyBuffer(uploadWriter, conn, *b)
		share.CloseWrite(hyConn)
	}()

	b := share.CopyBufPool.Get().(*[]byte)
	io.CopyBuffer(conn, downloadReader, *b)
	share.CopyBufPool.Put(b)
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

	for attempt := 0; attempt < 3; attempt++ {
		hyClient, err := getHyClientFromPool()
		if err != nil {
			log.Printf("[ERR] get hyClient from pool: %v", err)
			return nil, err
		}

		hyConn, err := hyClient.UDP()
		if err != nil {
			log.Printf("[ERR] dial UDP: %v", err)
			clientPoolMu.Lock()
			for i, c := range clientPool {
				if c == hyClient {
					c.Close()
					clientPool[i] = nil
					break
				}
			}
			clientPoolMu.Unlock()
			continue
		}

		if err := hyConn.Send(payload, ""); err != nil {
			log.Printf("[ERR] failed dns: %v", err)
			clientPoolMu.Lock()
			for i, c := range clientPool {
				if c == hyClient {
					c.Close()
					clientPool[i] = nil
					break
				}
			}
			clientPoolMu.Unlock()
			continue
		}

		respPayload, _, err := hyConn.Receive()
		if err != nil {
			log.Printf("[ERR] failed dns receive: %v", err)
			clientPoolMu.Lock()
			for i, c := range clientPool {
				if c == hyClient {
					c.Close()
					clientPool[i] = nil
					break
				}
			}
			clientPoolMu.Unlock()
			continue
		}

		respMsg := new(dns.Msg)
		if err := respMsg.Unpack(respPayload); err != nil {
			return nil, fmt.Errorf("unpack dns resp: %w", err)
		}

		return respMsg, nil
	}

	return nil, fmt.Errorf("failed to forward DNS after 3 attempts")
}

func getHyClientFromPool() (client.Client, error) {
	clientPoolMu.Lock()
	defer clientPoolMu.Unlock()

	if clientPool == nil {
		clientPool = make([]client.Client, hyClientPoolSize)
	}

	// Round-robin selection
	idx := poolIdx % hyClientPoolSize
	poolIdx++

	if clientPool[idx] != nil {
		return clientPool[idx], nil
	}

	connFactory := &udpConnFactory{}
	serverAddr, err := net.ResolveUDPAddr("udp", gServerAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve server addr: %w", err)
	}

	cidGen := &customCIDGenerator{authKey: gAuthKey}
	hyClient, _, err := client.NewClientWithConnectionIDGenerator(&client.Config{
		ConnFactory: connFactory,
		ServerAddr:  serverAddr,
		Auth:        cfg.Hysteria.AuthPassword,
		TLSConfig: client.TLSConfig{
			ServerName:         cfg.SNI,
			InsecureSkipVerify: true,
		},
	}, cidGen)
	if err != nil {
		return nil, fmt.Errorf("create pool hysteria client: %w", err)
	}

	clientPool[idx] = hyClient
	return hyClient, nil
}

func dialTCP(addr string) (net.Conn, error) {
	for attempt := 0; attempt < 3; attempt++ {
		hyClient, err := getHyClientFromPool()
		if err != nil {
			log.Printf("[ERR] get hyClient from pool: %v", err)
			return nil, err
		}

		tcpConn, err := hyClient.TCP(addr)
		if err != nil {
			log.Printf("[ERR] failed tcp to %s: %v", addr, err)
			clientPoolMu.Lock()
			for i, c := range clientPool {
				if c == hyClient {
					c.Close()
					clientPool[i] = nil
					break
				}
			}
			clientPoolMu.Unlock()
			continue
		}
		return tcpConn, nil
	}
	return nil, fmt.Errorf("failed to dial TCP after retries")
}

type udpConnFactory struct{}

func (f *udpConnFactory) New(serverAddr net.Addr) (net.PacketConn, error) {
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, err
	}
	return conn, nil
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
		return "", 0, 0, fmt.Errorf("unknown address type %d", atyp)
	}
	return host, port, dataStart, nil
}

// handleSocks5UDP handles a SOCKS5 UDP ASSOCIATE request.
// It creates a UDP session, binds a local UDP socket, and proxies SOCKS5 UDP datagrams bidirectionally.
func handleSocks5UDP(ctx context.Context, tcpConn net.Conn, dnsCache *storage.DnsCache) error {
	// Create UDP socket for relay
	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		tcpConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		log.Printf("[ERR] UDP ASSOCIATE listen error: %v", err)
		return fmt.Errorf("failed to create UDP socket: %w", err)
	}
	defer udpConn.Close()

	udpAddr := udpConn.LocalAddr().(*net.UDPAddr)
	reply := []byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, byte(udpAddr.Port >> 8), byte(udpAddr.Port)}
	if _, err := tcpConn.Write(reply); err != nil {
		return fmt.Errorf("failed to send UDP ASSOCIATE reply: %w", err)
	}

	log.Printf("[INFO] UDP relay active, local UDP: %s", udpAddr)

	var addrMu sync.Mutex
	var appAddr net.Addr

	// App → Server (Dynamic routing)
	go func() {
		buf := make([]byte, 65535)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			udpConn.SetReadDeadline(time.Now().Add(30 * time.Second))
			n, addr, err := udpConn.ReadFrom(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				return
			}

			// Track application address
			addrMu.Lock()
			if appAddr == nil {
				appAddr = addr
			}
			isApp := addr.String() == appAddr.String()
			addrMu.Unlock()

			if !isApp {
				// This is a direct response from a bypassed host
				addrMu.Lock()
				app := appAddr
				addrMu.Unlock()
				if app != nil {
					// Extract source address from the incoming packet
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

			// Not bypass. Send through hysteria UDP session.
			// Create a temporary UDP session for this request
			targetAddr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

			// Handle the UDP request through hysteria
			go handleUDPThroughHysteria(udpConn, appAddr, buf[dataStart:n], host, port, targetAddr)
		}
	}()

	// Keep TCP connection alive until client closes it
	_, err = io.Copy(io.Discard, tcpConn)
	return err
}

// handleUDPThroughHysteria handles UDP requests through hysteria tunnel using client pool with retries
func handleUDPThroughHysteria(udpConn net.PacketConn, appAddr net.Addr, data []byte, host string, port uint16, targetAddr string) {
	for attempt := 0; attempt < 3; attempt++ {
		// Get client from pool instead of creating new one
		hyClient, err := getHyClientFromPool()
		if err != nil {
			log.Printf("[ERR] get hyClient from pool: %v", err)
			return
		}

		hyConn, err := hyClient.UDP()
		if err != nil {
			log.Printf("[ERR] dial UDP: %v", err)
			// Remove client from pool and retry
			clientPoolMu.Lock()
			for i, c := range clientPool {
				if c == hyClient {
					c.Close()
					clientPool[i] = nil
					break
				}
			}
			clientPoolMu.Unlock()
			continue
		}

		// Send data through hysteria
		if err := hyConn.Send(data, targetAddr); err != nil {
			log.Printf("[ERR] failed udp send to %s: %v", targetAddr, err)
			// Remove client from pool and retry
			clientPoolMu.Lock()
			for i, c := range clientPool {
				if c == hyClient {
					c.Close()
					clientPool[i] = nil
					break
				}
			}
			clientPoolMu.Unlock()
			continue
		}

		// Set up response relay
		go func() {
			resp, _, err := hyConn.Receive()
			if err != nil {
				log.Printf("[ERR] failed udp receive: %v", err)
				return
			}

			// Wrap response in SOCKS5 format
			respHeader := []byte{0, 0, 0} // RSV(2), FRAG(1)
			if ip4 := net.ParseIP(host); ip4 != nil && ip4.To4() != nil {
				respHeader = append(respHeader, 0x01)
				respHeader = append(respHeader, ip4.To4()...)
			} else if ip6 := net.ParseIP(host); ip6 != nil {
				respHeader = append(respHeader, 0x04)
				respHeader = append(respHeader, ip6.To16()...)
			} else {
				// Domain name
				hostBytes := []byte(host)
				respHeader = append(respHeader, 0x03, byte(len(hostBytes)))
				respHeader = append(respHeader, hostBytes...)
			}
			var p [2]byte
			binary.BigEndian.PutUint16(p[:], uint16(port))
			respHeader = append(respHeader, p[:]...)
			respHeader = append(respHeader, resp...)

			udpConn.WriteTo(respHeader, appAddr)
		}()

		// Success, return
		return
	}

	log.Printf("[ERR] failed to handle UDP via hysteria after 3 attempts")
}
