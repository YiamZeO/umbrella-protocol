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
	"strconv"
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
	// quic-go использует это как подсказку для максимальной длины.
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
)

type tcpSession struct {
	tcpConn net.Conn
}

type hyUdpSession struct {
	hyConn     client.HyUDPConn
	lastActive time.Time
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
		go func() {
			shaper.RunShaperEngine(ctx)
		}()
	}

	if gDNSListen != "" {
		go func() {
			umbrella_dns.RunDNSServer(ctx, dnsCache, gBypass, gDNSListen, gDNSUpstream, forwardDNS)
		}()
	}

	ln, err := net.Listen("tcp", gListenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %v", cfg.ListenAddr, err)
	}
	defer ln.Close()

	log.Printf("[INFO] Umbrella/Hysteria client on %s (SOCKS5) → %s (SNI: %s)", cfg.ListenAddr, cfg.Server, cfg.SNI)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[INFO] Umbrella/Hysteria client listener closed, stopping")
			go func() {
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
			}()
			return nil
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
		go handleSOCKS5(ctx, conn, dnsCache)
	}
}

func listenSOCKS5(ctx context.Context, dnsCache *storage.DnsCache) {
}

func handleSOCKS5(ctx context.Context, conn net.Conn, dnsCache *storage.DnsCache) {
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

		if err := hyConn.Send(dnsData, gDNSUpstream); err != nil {
			log.Printf("[ERR] failed dns send: %v", err)
			hyConn.Close()
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

		receiveDone := make(chan struct{})
		var respPayload []byte
		var receiveErr error

		go func() {
			defer close(receiveDone)
			respPayload, _, receiveErr = hyConn.Receive()
		}()

		select {
		case <-receiveDone:
			hyConn.Close()
			if receiveErr != nil {
				log.Printf("[ERR] failed dns receive: %v", receiveErr)
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
		case <-time.After(10 * time.Second):
			hyConn.Close()
			log.Printf("[WARN] DNS receive timeout for %s", r.Question[0].Name)
			continue
		case <-ctx.Done():
			hyConn.Close()
			return nil, ctx.Err()
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

	if clientPool[idx] != nil && !clientPool[idx].IsClosed() {
		return clientPool[idx], nil
	}

	if clientPool[idx] != nil {
		clientPool[idx].Close()
		clientPool[idx] = nil
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

	var (
		addrMu      sync.Mutex
		appAddr     net.Addr
		sessions    = make(map[string]*hyUdpSession)
		sessionsMu  sync.Mutex
		sessionWait = make(map[string]chan struct{}) // To prevent duplicate session creation
	)

	// Session cleanup worker
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sessionsMu.Lock()
				for target, sess := range sessions {
					if time.Since(sess.lastActive) > 60*time.Second {
						log.Printf("[INFO] Closing idle Hysteria UDP session for %s", target)
						sess.hyConn.Close()
						delete(sessions, target)
					}
				}
				sessionsMu.Unlock()
			}
		}
	}()

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

			// Track application address dynamically (Discord might use multiple ports)
			addrMu.Lock()
			if appAddr == nil {
				appAddr = addr
				// log.Printf("[DEBUG] UDP relay: first app addr detected: %s", addr)
			}
			isApp := addr.String() == appAddr.String()
			// If not first app addr, check if it's still from localhost (possible second port from Discord)
			if !isApp {
				if udpAddr, ok := addr.(*net.UDPAddr); ok && udpAddr.IP.IsLoopback() {
					isApp = true
					// We don't update appAddr here to keep responses going to the primary port,
					// but SOCKS5 UDP usually expects responses to the port that sent the packet.
				}
			}
			addrMu.Unlock()

			if !isApp {
				// Response from direct/bypassed host - send back to the last known app port
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

			// From app. Parse destination.
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
				targetAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
				if err == nil {
					udpConn.WriteTo(buf[dataStart:n], targetAddr)
				}
				continue
			}

			// Tunnel through Hysteria
			targetAddr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

			sessionsMu.Lock()
			sess, ok := sessions[targetAddr]
			if ok {
				sess.lastActive = time.Now()
				if err := sess.hyConn.Send(buf[dataStart:n], targetAddr); err != nil {
					log.Printf("[ERR] Cached Hysteria UDP send failed: %v", err)
					sess.hyConn.Close()
					delete(sessions, targetAddr)
					ok = false
				}
			}

			if !ok {
				waitCh, busy := sessionWait[targetAddr]
				if busy {
					sessionsMu.Unlock()
					go func(data []byte) {
						<-waitCh
						sessionsMu.Lock()
						if s, stillOk := sessions[targetAddr]; stillOk {
							s.lastActive = time.Now()
							s.hyConn.Send(data, targetAddr)
						}
						sessionsMu.Unlock()
					}(append([]byte(nil), buf[dataStart:n]...))
					continue
				}

				waitCh = make(chan struct{})
				sessionWait[targetAddr] = waitCh
				sessionsMu.Unlock()

				go func(data []byte, t string, sender net.Addr) {
					defer func() {
						sessionsMu.Lock()
						delete(sessionWait, t)
						close(waitCh)
						sessionsMu.Unlock()
					}()

					for attempt := 0; attempt < 3; attempt++ {
						hyClient, err := getHyClientFromPool()
						if err != nil {
							log.Printf("[ERR] get hyClient from pool: %v", err)
							continue
						}
						hyConn, err := hyClient.UDP()
						if err != nil {
							log.Printf("[ERR] get hyConn from hyClient: %v", err)
							continue
						}
						if err := hyConn.Send(data, t); err != nil {
							log.Printf("[ERR] failed udp send to %s: %v", targetAddr, err)
							hyConn.Close()
							continue
						}

						newSess := &hyUdpSession{
							hyConn:     hyConn,
							lastActive: time.Now(),
						}
						sessionsMu.Lock()
						sessions[t] = newSess
						sessionsMu.Unlock()

						// Response listener loop
						go func() {
							for {
								resp, from, err := hyConn.Receive()
								if err != nil {
									log.Printf("[ERR] failed udp receive: %v", err)
									return
								}
								sessionsMu.Lock()
								if s, stillExists := sessions[t]; stillExists && s.hyConn == hyConn {
									s.lastActive = time.Now()
								}
								sessionsMu.Unlock()

								// Wrap in SOCKS5 with ACTUAL source address
								respHeader := []byte{0, 0, 0}

								// Parse 'from' address (it can be different from 't')
								hostStr, portStr, _ := net.SplitHostPort(from)
								fromPort, _ := strconv.ParseUint(portStr, 10, 16)

								if ip := net.ParseIP(hostStr); ip != nil {
									if ip4 := ip.To4(); ip4 != nil {
										respHeader = append(respHeader, 0x01)
										respHeader = append(respHeader, ip4...)
									} else {
										respHeader = append(respHeader, 0x04)
										respHeader = append(respHeader, ip.To16()...)
									}
								} else {
									respHeader = append(respHeader, 0x03, byte(len(hostStr)))
									respHeader = append(respHeader, []byte(hostStr)...)
								}
								var pb [2]byte
								binary.BigEndian.PutUint16(pb[:], uint16(fromPort))
								respHeader = append(respHeader, pb[:]...)
								respHeader = append(respHeader, resp...)

								udpConn.WriteTo(respHeader, sender)
							}
						}()
						return
					}
				}(append([]byte(nil), buf[dataStart:n]...), targetAddr, addr)
			} else {
				sessionsMu.Unlock()
			}
		}
	}()

	// Keep TCP connection alive until client closes it
	_, err = io.Copy(io.Discard, tcpConn)
	return err
}

// handleUDPThroughHysteria is no longer used individually as logic moved to handleSocks5UDP for session caching
func handleUDPThroughHysteria(udpConn net.PacketConn, appAddr net.Addr, data []byte, host string, port uint16, targetAddr string) {
}
