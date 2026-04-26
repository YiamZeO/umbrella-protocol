package client

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"

	"umbrella_client/internal/storage"
)

// runDNSServer starts a local DNS server that resolves queries via the tunnel.
// It populates dnsCache with IP -> Hostname mappings to fix bypass-by-host.
func runDNSServer(ctx context.Context, dnsCache *storage.DnsCache) {
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		msg := new(dns.Msg)
		msg.SetReply(r)
		msg.Authoritative = true

		if len(r.Question) == 0 {
			w.WriteMsg(msg)
			return
		}

		q := r.Question[0]
		hostname := strings.TrimSuffix(q.Name, ".")

		// Should we resolve this via a separate bypass DNS?
		useBypassDNS := shouldBypass(hostname)

		// Only handle A (IPv4) and AAAA (IPv6) for caching
		if q.Qtype != dns.TypeA && q.Qtype != dns.TypeAAAA {
			// Forward other types
			var resp *dns.Msg
			var err error
			if useBypassDNS {
				resp, err = resolveDirectDNS(r)
			} else {
				resp, err = forwardDNS(ctx, r)
			}
			if err == nil {
				w.WriteMsg(resp)
			}
			return
		}

		var resp *dns.Msg
		var err error
		if useBypassDNS {
			resp, err = resolveDirectDNS(r)
		} else {
			resp, err = forwardDNS(ctx, r)
		}

		if err != nil {
			log.Printf("[ERR] DNS: resolution error for %s (bypassDNS=%v): %v", hostname, useBypassDNS, err)
			dns.HandleFailed(w, r)
			return
		}

		// Cache the results: IP -> Hostname
		for _, ans := range resp.Answer {
			var ip string
			if a, ok := ans.(*dns.A); ok {
				ip = a.A.String()
			} else if aaaa, ok := ans.(*dns.AAAA); ok {
				ip = aaaa.AAAA.String()
			}
			if ip != "" {
				dnsCache.Store(ip, hostname)
				dnsCache.SetRevertValue(hostname, ip)
				log.Printf("DNS: resolution success for %s (bypassDNS=%v): %s", hostname, useBypassDNS, ip)
				break
			}
		}

		w.WriteMsg(resp)
	})

	server := &dns.Server{
		Addr:    gDNSListen,
		Net:     "udp",
		Handler: handler,
	}

	log.Printf("DNS server listening on %s (upstream: %s)", gDNSListen, gDNSUpstream)

	go func() {
		<-ctx.Done()
		log.Printf("DNS: shutting down server...")
		server.Shutdown()
	}()

	if err := server.ListenAndServe(); err != nil {
		select {
		case <-ctx.Done():
			log.Printf("DNS: server stopped")
		default:
			log.Printf("[ERR] DNS server error: %v", err)
		}
	}
}

// resolveDirectDNS отправляет DNS-запрос через системный резолвер ОС
func resolveDirectDNS(r *dns.Msg) (*dns.Msg, error) {
	// 1. Проверяем наличие вопросов в пакете
	if len(r.Question) == 0 {
		return nil, fmt.Errorf("empty DNS question")
	}

	domain := r.Question[0].Name // Берём запрашиваемый домен
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 2. Создаём резолвер
	resolver := &net.Resolver{}
	resolver.PreferGo = false

	// 3. Выполняем системный резолвинг
	ips, err := resolver.LookupIPAddr(ctx, domain)
	if err != nil {
		return nil, fmt.Errorf("system resolve failed: %w", err)
	}

	// 4. Формируем ответ в формате *dns.Msg
	resp := new(dns.Msg)
	resp.SetReply(r)
	resp.Authoritative = false

	for _, ip := range ips {
		// Проверяем тип IP и добавляем соответствующую запись в массив Answer
		if ip4 := ip.IP.To4(); ip4 != nil {
			resp.Answer = append(resp.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: domain, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
				A:   ip4,
			})
		} else if ip6 := ip.IP.To16(); ip6 != nil {
			resp.Answer = append(resp.Answer, &dns.AAAA{
				Hdr:  dns.RR_Header{Name: domain, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 300},
				AAAA: ip6,
			})
		}
	}

	return resp, nil
}

// forwardDNS sends a DNS query to the upstream server via the Umbrella tunnel.
func forwardDNS(ctx context.Context, r *dns.Msg) (*dns.Msg, error) {
	s, err := getSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}

	stream, err := openUDPStream(s)
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

	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(payload)))

	if _, err := stream.Write(lenBuf); err != nil {
		return nil, err
	}
	if _, err := stream.Write(payload); err != nil {
		return nil, err
	}

	// Read response
	if _, err := io.ReadFull(stream, lenBuf); err != nil {
		return nil, err
	}
	respLen := binary.BigEndian.Uint32(lenBuf)
	respPayload := make([]byte, respLen)
	if _, err := io.ReadFull(stream, respPayload); err != nil {
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
