package xtls

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
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
// SmartShaper: caller оборачивает dst в downWriter/shapedWriter до передачи сюда.

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

// visionRandPadLen возвращает случайную длину padding в [0, maxVisionPadding].
func visionRandPadLen() (uint16, error) {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return uint16(b[0]), nil
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

	// 1. Write Fake TLS Header (5 bytes)
	// Total length = 2 (for padLen field) + padLen + len(payload)
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
