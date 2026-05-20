package xtls

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// Vision — слой фрейминга поверх yamux-потока, скрывающий сигнатуру TLS-in-TLS.
//
// Все TLS record'ы (handshake, CCS, Alert и Application Data) оборачиваются
// в Vision-фрейм:
//
//	[2 bytes: padding_len, big-endian]   ← случайное 0..255
//	[padding_len bytes: random noise]
//	[5 bytes: оригинальный TLS record header]
//	[body_len bytes: оригинальный TLS record body]
//
// SmartShaper применяется на уровне writer'а до передачи в Vision:
// caller сам оборачивает dst в shapedWriter — vision.go этого не знает.

const (
	maxVisionPadding = 255
)

// visionCopyToTunnel reads raw data from src, wraps each chunk in a Vision frame with padding,
// and writes it to dst. This version is universal and doesn't assume src is TLS.
func visionCopyToTunnel(dst io.Writer, src io.Reader) error {
	br := bufio.NewReaderSize(src, 32*1024)
	buf := make([]byte, 16*1024) // Chunk size for framing
	hdr := make([]byte, 5)
	framePfx := make([]byte, 2)
	padBuf := make([]byte, 256) // Pre-allocated padding buffer

	for {
		// Read whatever is available (up to 16KB)
		n, err := br.Read(buf)
		if err != nil {
			return err
		}

		payload := buf[:n]
		padLen, err := visionRandPadLen()
		if err != nil {
			return fmt.Errorf("vision rand: %w", err)
		}

		// 1. Write Fake TLS Header (5 bytes)
		hdr[0] = 0x17
		hdr[1] = 0x03
		hdr[2] = 0x03
		binary.BigEndian.PutUint16(hdr[3:5], uint16(2+int(padLen)+len(payload)))
		if _, err := dst.Write(hdr); err != nil {
			return err
		}

		// 2. Write PadLen field (2 bytes)
		binary.BigEndian.PutUint16(framePfx, padLen)
		if _, err := dst.Write(framePfx); err != nil {
			return err
		}

		// 3. Write Padding (random noise)
		if padLen > 0 {
			pad := padBuf[:padLen]
			if _, err := rand.Read(pad); err != nil {
				return fmt.Errorf("vision rand pad: %w", err)
			}
			if _, err := dst.Write(pad); err != nil {
				return err
			}
		}

		// 4. Write Payload
		if _, err := dst.Write(payload); err != nil {
			return err
		}
	}
}

// visionCopyFromTunnel reads Vision-framed data from src, removes padding and fake headers,
// and writes the raw inner data to dst.
func visionCopyFromTunnel(dst io.Writer, src io.Reader) error {
	pfxBuf := make([]byte, 2)
	hdr := make([]byte, 5)

	for {
		// 1. Read Fake TLS header (5 bytes)
		if _, err := io.ReadFull(src, hdr); err != nil {
			return err
		}
		totalLen := int(binary.BigEndian.Uint16(hdr[3:5]))

		// 2. Read PadLen (2 bytes)
		if _, err := io.ReadFull(src, pfxBuf); err != nil {
			return err
		}
		padLen := int(binary.BigEndian.Uint16(pfxBuf))

		// 3. Skip Padding
		if padLen > 0 {
			if _, err := io.CopyN(io.Discard, src, int64(padLen)); err != nil {
				return err
			}
		}

		// 4. Read ONLY the actual payload
		payloadLen := totalLen - 2 - padLen
		if payloadLen < 0 {
			return fmt.Errorf("vision: invalid frame length")
		}
		if _, err := io.CopyN(dst, src, int64(payloadLen)); err != nil {
			return err
		}
	}
}

// visionRandPadLen возвращает случайную длину padding в [0, 255].
func visionRandPadLen() (uint16, error) {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return uint16(b[0]), nil // 0..255
}

// VisionUDPFrame - Vision-фрейм для UDP датаграмм:
// [2 bytes: padLen, big-endian]
// [padLen bytes: random noise]
// [4 bytes: payload length, big-endian]
// [payloadLen bytes: UDP payload]

// visionReadDatagram читает один Vision-UDP фрейм из src и возвращает raw payload.
func visionReadDatagram(src io.Reader) ([]byte, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(src, hdr); err != nil {
		return nil, fmt.Errorf("read vision udp hdr: %w", err)
	}
	totalLen := int(binary.BigEndian.Uint16(hdr[3:5]))

	pfxBuf := make([]byte, 2)
	if _, err := io.ReadFull(src, pfxBuf); err != nil {
		return nil, fmt.Errorf("read padLen: %w", err)
	}
	padLen := int(binary.BigEndian.Uint16(pfxBuf))

	if padLen > 0 {
		if _, err := io.CopyN(io.Discard, src, int64(padLen)); err != nil {
			return nil, fmt.Errorf("skip padding: %w", err)
		}
	}

	payloadLen := totalLen - 2 - padLen
	if payloadLen < 0 {
		return nil, fmt.Errorf("vision udp: invalid frame length")
	}

	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(src, payload); err != nil {
		return nil, fmt.Errorf("read payload: %w", err)
	}

	return payload, nil
}

// visionWriteDatagram записывает один Vision-UDP фрейм (Header + Padding + Payload) в dst.
func visionWriteDatagram(dst io.Writer, payload []byte) error {
	padLen, err := visionRandPadLen()
	if err != nil {
		return fmt.Errorf("vision rand: %w", err)
	}

	// Ensure total length (2 + padLen + len(payload)) doesn't exceed 65535 (uint16 limit).
	// Most UDP packets are ~1400-1500 bytes, but standard max is 65507.
	if 2+int(padLen)+len(payload) > 65535 {
		if len(payload) > 65533 {
			padLen = 0
			// If payload itself is > 65535, it will be truncated by uint16 cast.
			// But in SOCKS5 UDP, payload is typically <= 65507 + headers.
		} else {
			padLen = uint16(65535 - 2 - len(payload))
		}
	}

	// 1. Write Fake TLS Header (5 bytes)
	hdr := make([]byte, 5)
	hdr[0] = 0x17
	hdr[1] = 0x03
	hdr[2] = 0x03
	binary.BigEndian.PutUint16(hdr[3:5], uint16(2+int(padLen)+len(payload)))
	if _, err := dst.Write(hdr); err != nil {
		return fmt.Errorf("write hdr: %w", err)
	}

	// 2. Write PadLen field (2 bytes)
	pfxBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(pfxBuf, padLen)
	if _, err := dst.Write(pfxBuf); err != nil {
		return fmt.Errorf("write padLen: %w", err)
	}

	// 3. Write Padding (random noise)
	if padLen > 0 {
		pad := make([]byte, padLen)
		if _, err := rand.Read(pad); err != nil {
			return fmt.Errorf("write padding: %w", err)
		}
		if _, err := dst.Write(pad); err != nil {
			return fmt.Errorf("write padding: %w", err)
		}
	}

	// 4. Write Payload
	if _, err := dst.Write(payload); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}

	return nil
}

// peekConn оборачивает net.Conn и воспроизводит один уже прочитанный байт
// перед последующими вызовами Read. Используется для детектирования TLS
// без потребления первого байта из потока.
type peekConn struct {
	net.Conn
	peeked  byte
	hasPeek bool
}

func (pc *peekConn) Read(p []byte) (int, error) {
	if pc.hasPeek {
		p[0] = pc.peeked
		pc.hasPeek = false
		if len(p) == 1 {
			return 1, nil
		}
		n, err := pc.Conn.Read(p[1:])
		return n + 1, err
	}
	return pc.Conn.Read(p)
}

// peekOneByte читает один байт из conn без «потребления»:
// возвращает peekConn, у которого этот байт будет первым при Read.
func peekOneByte(conn net.Conn) (*peekConn, byte, error) {
	pc := &peekConn{Conn: conn}
	var b [1]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil {
		return nil, 0, err
	}
	pc.peeked = b[0]
	pc.hasPeek = true
	return pc, b[0], nil
}

// visionConn оборачивает yamux.Stream и прозрачно выполняет Vision-фрейминг.
type visionConn struct {
	net.Conn
	appReader *io.PipeReader
	appWriter *io.PipeWriter
	closeOnce sync.Once
}

func (v *visionConn) Read(p []byte) (int, error) {
	if gConnectionsTimeOut > 0 {
		v.Conn.SetReadDeadline(time.Now().Add(gConnectionsTimeOut))
	}
	return v.appReader.Read(p)
}

func (v *visionConn) Write(p []byte) (int, error) {
	if gConnectionsTimeOut > 0 {
		v.Conn.SetWriteDeadline(time.Now().Add(gConnectionsTimeOut))
	}
	return v.appWriter.Write(p)
}

func (v *visionConn) CloseWrite() error {
	return v.appWriter.Close()
}

func (v *visionConn) Close() error {
	v.closeOnce.Do(func() {
		v.appWriter.Close()
		v.appReader.Close()
	})
	return v.Conn.Close()
}

// openVisionStream opens a yamux stream for Vision TCP tunnel.
// Uses cmd=0x00 for Vision framing.
func openVisionStream(s *yamux.Session, destHost string, destPort uint16) (net.Conn, error) {
	stream, err := s.Open()
	if err != nil {
		for i := 0; i < gSessionsNum; i++ {
			sessMu[i].Lock()
			if sessions[i] == s {
				sessions[i] = nil
			}
			sessMu[i].Unlock()
		}
		return nil, err
	}

	var addrBytes []byte
	// Normalize: net.ParseIP doesn't like brackets
	cleanHost := strings.Trim(destHost, "[]")
	ip := net.ParseIP(cleanHost)
	if ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			addrBytes = append([]byte{0x01}, ip4...)
		} else {
			addrBytes = append([]byte{0x04}, ip.To16()...)
		}
	} else {
		if len(destHost) > 255 {
			go stream.Close()
			return nil, fmt.Errorf("domain name too long: %d bytes", len(destHost))
		}
		addrBytes = append([]byte{0x03, byte(len(destHost))}, []byte(destHost)...)
	}

	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], destPort)

	req := append([]byte{0x00}, addrBytes...)
	req = append(req, portBytes[:]...)
	if gConnectionsTimeOut > 0 {
		stream.SetWriteDeadline(time.Now().Add(gConnectionsTimeOut))
	}
	if _, err := stream.Write(req); err != nil {
		go stream.Close()
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			dropStalledSession(s)
		}
		return nil, fmt.Errorf("write vision request: %w", err)
	}

	var respBuf [1]byte
	if gConnectionsTimeOut > 0 {
		stream.SetReadDeadline(time.Now().Add(gConnectionsTimeOut))
	}
	if _, err := io.ReadFull(stream, respBuf[:]); err != nil {
		go stream.Close()
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			dropStalledSession(s)
		}
		return nil, fmt.Errorf("read vision response: %w", err)
	}
	if respBuf[0] != 0x00 {
		go stream.Close()
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			dropStalledSession(s)
		}
		return nil, fmt.Errorf("server rejected vision connection to %s:%d", destHost, destPort)
	}

	appRead, visionWrite := io.Pipe()
	visionRead, appWrite := io.Pipe()

	vConn := &visionConn{
		Conn:      &timeoutConn{Conn: stream},
		appReader: appRead,
		appWriter: appWrite,
	}

	go func() {
		visionCopyToTunnel(stream, visionRead)
		if hc, ok := stream.(interface{ CloseWrite() error }); ok {
			hc.CloseWrite()
		} else {
			stream.Close()
		}
	}()

	go func() {
		visionCopyFromTunnel(visionWrite, stream)
		visionWrite.Close()
	}()

	return vConn, nil
}
