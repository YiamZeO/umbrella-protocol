package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
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
// SmartShaper: caller оборачивает dst в downWriter/shapedWriter до передачи сюда.

const (
	visionSentinel   = uint16(0xFFFF)
	maxVisionPadding = 255
)

// visionCopyToTunnel читает raw inner TLS из src, оборачивает каждый record
// Vision-фреймом, шлёт в dst. После первого AppData шлёт sentinel + splice.
// Для throttle (SmartShaper ↓): передавайте dst уже обёрнутым в downWriter.
func visionCopyToTunnel(dst io.Writer, src io.Reader) error {
	hdr := make([]byte, 5)
	sentinel := [2]byte{0xFF, 0xFF}
	framePfx := make([]byte, 2)

	for {
		if _, err := io.ReadFull(src, hdr); err != nil {
			return err
		}
		recordType := hdr[0]
		bodyLen := int(binary.BigEndian.Uint16(hdr[3:5]))

		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(src, body); err != nil {
			return err
		}

		if recordType == 0x17 { // Application Data → sentinel + splice
			if _, err := dst.Write(sentinel[:]); err != nil {
				return err
			}
			if _, err := dst.Write(hdr); err != nil {
				return err
			}
			if _, err := dst.Write(body); err != nil {
				return err
			}
			_, err := io.Copy(dst, src)
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
// снимает padding, после sentinel переключается на splice. Пишет raw TLS в dst.
func visionCopyFromTunnel(dst io.Writer, src io.Reader) error {
	pfxBuf := make([]byte, 2)
	hdr := make([]byte, 5)

	for {
		if _, err := io.ReadFull(src, pfxBuf); err != nil {
			return err
		}
		padLen := binary.BigEndian.Uint16(pfxBuf)

		if padLen == visionSentinel {
			_, err := io.Copy(dst, src)
			return err
		}

		if padLen > 0 {
			discard := make([]byte, padLen)
			if _, err := io.ReadFull(src, discard); err != nil {
				return err
			}
		}

		if _, err := io.ReadFull(src, hdr); err != nil {
			return err
		}
		bodyLen := int(binary.BigEndian.Uint16(hdr[3:5]))
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(src, body); err != nil {
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

// visionRandPadLen возвращает случайную длину padding в [0, maxVisionPadding].
func visionRandPadLen() (uint16, error) {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return uint16(b[0]), nil
}
