package umbrella_dns

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"

	"umbrella_client/internal/storage"
)

// ShouldBypass returns true if the host should be connected to directly.
// Supports domains (including all subdomains) and IP CIDR ranges (e.g. "192.168.1.0/24").
func ShouldBypass(host string, gBypass []string) bool {
	if len(gBypass) == 0 {
		return false
	}

	host = strings.ToLower(host)
	hostIP := net.ParseIP(host)

	for _, rule := range gBypass {
		rule = strings.ToLower(rule)
		if rule == "" {
			continue
		}

		// 1. Try as CIDR (e.g. "192.168.1.0/24")
		if hostIP != nil {
			if _, ipnet, err := net.ParseCIDR(rule); err == nil {
				if ipnet.Contains(hostIP) {
					return true
				}
				continue
			}
		}

		// 2. Exact match (works for both IPs and domains)
		if host == rule {
			return true
		}

		// 3. Domain suffix match (only if host is not an IP)
		if hostIP == nil && strings.HasSuffix(host, "."+rule) {
			return true
		}
	}
	return false
}

// RunDNSServer starts a local DNS server that resolves queries via the tunnel.
// It populates dnsCache with IP -> Hostname mappings to fix bypass-by-host.
func RunDNSServer(ctx context.Context, dnsCache *storage.DnsCache, gBypass []string, gDNSListen, gDNSUpstream string, forwardDNS func(ctx context.Context, r *dns.Msg) (*dns.Msg, error)) {
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
		useBypassDNS := ShouldBypass(hostname, gBypass)

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
				log.Printf("[INFO] DNS: resolution success for %s (bypassDNS=%v): %s", hostname, useBypassDNS, ip)
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

	log.Printf("[INFO] DNS server listening on %s (upstream: %s)", gDNSListen, gDNSUpstream)

	go func() {
		<-ctx.Done()
		log.Printf("[INFO] DNS: shutting down server...")
		server.Shutdown()
	}()

	if err := server.ListenAndServe(); err != nil {
		select {
		case <-ctx.Done():
			log.Printf("[INFO] DNS: server stopped")
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
