package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

func main() {
	serverAddrStr := "138.124.240.89:443"
	timeout := 10 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true, // Для теста проксирования обычно ставим true, если не уверены в CA
		ServerName:        "cloudflare.com",
		NextProtos:        []string{"h3", "h2", "http/1.1"},
	}

	quicConfig := &quic.Config{
		MaxIdleTimeout: timeout,
		KeepAlivePeriod: time.Second,
	}

	udpConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		log.Fatalf("create UDP listener: %v", err)
	}
	defer udpConn.Close()

	serverAddr, err := net.ResolveUDPAddr("udp", serverAddrStr)
	if err != nil {
		log.Fatalf("resolve server addr: %v", err)
	}

	conn, err := quic.Dial(ctx, udpConn, serverAddr, tlsConfig, quicConfig)
	if err != nil {
		log.Fatalf("QUIC dial failed: %v", err)
	}
	defer conn.CloseWithError(0, "test done")

	connState := conn.ConnectionState()
	tlsState := connState.TLS

	fmt.Printf("=== QUIC Connection Info ===\n")
	fmt.Printf("Server Address: %s\n", serverAddrStr)
	fmt.Printf("Actual RemoteAddr: %s\n", conn.RemoteAddr())
	fmt.Printf("QUIC Version: %s\n", connState.Version.String())
	fmt.Printf("TLS Version: %s\n", tlsVersionToString(tlsState.Version))
	fmt.Printf("Cipher Suite: %s\n", tls.CipherSuiteName(tlsState.CipherSuite))
	fmt.Printf("Server Name (SNI): %s\n", tlsState.ServerName)

	if len(tlsState.PeerCertificates) > 0 {
		cert := tlsState.PeerCertificates[0]
		fmt.Printf("\n=== Server Certificate ===\n")
		fmt.Printf("Subject: %s\n", cert.Subject)
		fmt.Printf("Issuer: %s\n", cert.Issuer)
		fmt.Printf("Not Before: %s\n", cert.NotBefore)
		fmt.Printf("Not After: %s\n", cert.NotAfter)
		fmt.Printf("DNS Names: %v\n", cert.DNSNames)
		if cert.SerialNumber != nil {
			fmt.Printf("Serial: %s\n", cert.SerialNumber.String())
		}
	}

	fmt.Println("\n=== Test Passed: Connection established and certificate received ===")
}

func tlsVersionToString(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLS 1.0"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return fmt.Sprintf("Unknown (0x%04x)", v)
	}
}
