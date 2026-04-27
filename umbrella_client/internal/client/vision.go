package client

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// Vision — слой фрейминга поверх yamux-потока, скрывающий сигнатуру TLS-in-TLS.
//
// Фаза Handshake (record types 0x14, 0x15, 0x16):
// каждый inner TLS record оборачивается в Vision-фрейм:
//
//	[2 bytes: padding_len, big-endian]   ← случайное 0..255
//	[padding_len bytes: random noise]
//	[5 bytes: оригинальный TLS record header]
//	[body_len bytes: оригинальный TLS record body]
//
// Фаза Splice (после первого Application Data, type 0x17):
// writer шлёт sentinel [0xFF 0xFF], затем переключается на io.Copy.
// Reader видит sentinel и тоже переключается на io.Copy.
//
// Каждое направление (upload / download) переключается независимо.
// SmartShaper применяется на уровне writer'а до передачи в Vision:
// caller сам оборачивает dst в shapedWriter — vision.go этого не знает.

const (
	visionSentinel = uint16(0xFFFF)
)

// visionCopyToTunnel читает raw inner TLS из src (app-соединение или remote),
// обворачивает каждый record Vision-фреймом, шлёт в dst (yamux stream или conn).
// После первого AppData отправляет sentinel и переходит в splice (io.Copy).
// SmartShaper: передайте dst уже обёрнутым в shapedWriter, если нужен throttle.
func visionCopyToTunnel(dst io.Writer, src io.Reader) error {
	br := bufio.NewReaderSize(src, 32*1024)
	hdr := make([]byte, 5)
	sentinel := [2]byte{0xFF, 0xFF}
	framePfx := make([]byte, 2)

	for {
		// Читаем TLS record header (type 1 + ver 2 + len 2 = 5 байт).
		if _, err := io.ReadFull(br, hdr); err != nil {
			return err
		}
		recordType := hdr[0]
		bodyLen := int(binary.BigEndian.Uint16(hdr[3:5]))

		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(br, body); err != nil {
			return err
		}

		if recordType == 0x17 { // Application Data → sentinel + splice
			if _, err := dst.Write(sentinel[:]); err != nil {
				return err
			}
			// Первую AppData запись шлём raw.
			if _, err := dst.Write(hdr); err != nil {
				return err
			}
			if _, err := dst.Write(body); err != nil {
				return err
			}
			_, err := io.Copy(dst, br)
			return err
		}

		// Handshake / CCS / Alert → Vision-фрейм.
		padLen, err := visionRandPadLen()
		if err != nil {
			return fmt.Errorf("vision rand: %w", err)
		}
		binary.BigEndian.PutUint16(framePfx, padLen)
		if _, err := dst.Write(framePfx); err != nil {
			return err
		}
		if padLen > 0 {
			pad := make([]byte, padLen)
			if _, err := rand.Read(pad); err != nil {
				return fmt.Errorf("vision rand pad: %w", err)
			}
			if _, err := dst.Write(pad); err != nil {
				return err
			}
		}
		if _, err := dst.Write(hdr); err != nil {
			return err
		}
		if _, err := dst.Write(body); err != nil {
			return err
		}
	}
}

// visionCopyFromTunnel читает Vision-фреймированные данные из src (yamux stream),
// снимает padding в фазе Handshake, после sentinel переключается на splice.
// Пишет raw inner TLS в dst (app-соединение).
func visionCopyFromTunnel(dst io.Writer, src io.Reader) error {
	br := bufio.NewReaderSize(src, 32*1024)
	pfxBuf := make([]byte, 2)
	hdr := make([]byte, 5)

	for {
		if _, err := io.ReadFull(br, pfxBuf); err != nil {
			return err
		}
		padLen := binary.BigEndian.Uint16(pfxBuf)

		if padLen == visionSentinel {
			// Splice: остальное идёт raw.
			_, err := io.Copy(dst, br)
			return err
		}

		// Отбрасываем padding.
		if padLen > 0 {
			discard := make([]byte, padLen)
			if _, err := io.ReadFull(br, discard); err != nil {
				return err
			}
		}

		// Читаем TLS record header и body.
		if _, err := io.ReadFull(br, hdr); err != nil {
			return err
		}
		bodyLen := int(binary.BigEndian.Uint16(hdr[3:5]))
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(br, body); err != nil {
			return err
		}

		if _, err := dst.Write(hdr); err != nil {
			return err
		}
		if _, err := dst.Write(body); err != nil {
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

// openVisionStream открывает yamux-поток типа 0x03 (Vision TCP tunnel).
// Протокол совпадает с openStream, но первый байт команды = 0x03.
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
	ip := net.ParseIP(destHost)
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

	req := append([]byte{0x03}, addrBytes...) // cmd = 0x03 (Vision)
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
		Conn:      &timeoutStream{Conn: stream},
		appReader: appRead,
		appWriter: appWrite,
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		visionCopyToTunnel(stream, visionRead)
		if hc, ok := stream.(interface{ CloseWrite() error }); ok {
			hc.CloseWrite()
		} else {
			stream.Close()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		visionCopyFromTunnel(visionWrite, stream)
		visionWrite.Close()
	}()

	return vConn, nil
}
