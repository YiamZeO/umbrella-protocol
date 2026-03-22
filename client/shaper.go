package main

import (
	"encoding/binary"
	"io"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// gUpBucket is the current upload rate bucket, atomically replaced on each phase change.
// nil means no throttling (default/fallback behaviour).
var gUpBucket atomic.Pointer[rateBucket]

// rateBucket is a token-bucket rate limiter with a cancellable sleep.
// When paused (bytesPerSec == 0), writes block entirely until the phase changes or expires.
type rateBucket struct {
	paused   bool
	mu       sync.Mutex
	tokens   float64
	rate     float64 // bytes per nanosecond
	capacity float64
	last     time.Time
	done     chan struct{} // closed when this bucket is superseded
}

func newRateBucket(bytesPerSec float64) *rateBucket {
	b := &rateBucket{done: make(chan struct{})}
	if bytesPerSec <= 0 {
		b.paused = true
	} else {
		b.rate = bytesPerSec / 1e9
		b.capacity = bytesPerSec
		b.tokens = bytesPerSec
		b.last = time.Now()
	}
	return b
}

// wait consumes n tokens, sleeping as needed.
// Returns false if the bucket was superseded — caller retries with new bucket.
func (b *rateBucket) wait(n int64) bool {
	if b.paused {
		<-b.done
		return false
	}
	b.mu.Lock()
	now := time.Now()
	b.tokens += float64(now.Sub(b.last).Nanoseconds()) * b.rate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.last = now
	var sleep time.Duration
	if float64(n) <= b.tokens {
		b.tokens -= float64(n)
	} else {
		deficit := float64(n) - b.tokens
		b.tokens = 0
		sleep = time.Duration(deficit / b.rate)
	}
	b.mu.Unlock()
	if sleep > 0 {
		select {
		case <-time.After(sleep):
		case <-b.done:
			return false
		}
	}
	return true
}

// phaseCancel tracks the stop channel for the currently active phase timer.
var (
	phaseCancel   chan struct{}
	phaseCancelMu sync.Mutex
)

// cancelCurrentPhase stops the active phase timer goroutine if any.
func cancelCurrentPhase() {
	phaseCancelMu.Lock()
	defer phaseCancelMu.Unlock()
	if phaseCancel != nil {
		close(phaseCancel)
		phaseCancel = nil
	}
}

// readPhaseUpdates reads full phase parameters from controlStream sent by the server
// and updates the upload bucket accordingly. Each message is 12 bytes:
//
//	[0:4]  duration_ms  uint32, big-endian
//	[4:8]  down_mbps    float32 bits, big-endian (server-side only, ignored here)
//	[8:12] up_mbps      float32 bits, big-endian
//
// The client sets a local timer for duration_ms; when it fires, the bucket is cleared
// and uploads fall back to default (no throttle). A new phase message cancels the previous timer.
func readPhaseUpdates(controlStream io.ReadCloser) {
	defer func() { go controlStream.Close() }()
	buf := make([]byte, 12)
	for {
		if _, err := io.ReadFull(controlStream, buf); err != nil {
			cancelCurrentPhase()
			if old := gUpBucket.Swap(nil); old != nil {
				close(old.done)
			}
			return
		}
		durationMs := binary.BigEndian.Uint32(buf[0:4])
		downMbps := math.Float32frombits(binary.BigEndian.Uint32(buf[4:8]))
		upMbps := math.Float32frombits(binary.BigEndian.Uint32(buf[8:12]))

		cancelCurrentPhase()

		newBucket := newRateBucket(float64(upMbps) * 125_000)
		if old := gUpBucket.Swap(newBucket); old != nil {
			close(old.done)
		}

		cancelCh := make(chan struct{})
		phaseCancelMu.Lock()
		phaseCancel = cancelCh
		phaseCancelMu.Unlock()

		dur := time.Duration(durationMs) * time.Millisecond
		bucket := newBucket // capture for CAS
		go func() {
			select {
			case <-time.After(dur):
				// Phase expired: revert to no-throttle only if our bucket is still active.
				if gUpBucket.CompareAndSwap(bucket, nil) {
					close(bucket.done)
					log.Printf("[shaper] phase expired → fallback (no throttle)")
				}
			case <-cancelCh:
				// New phase arrived before this one expired.
			}
			phaseCancelMu.Lock()
			if phaseCancel == cancelCh {
				phaseCancel = nil
			}
			phaseCancelMu.Unlock()
		}()
		log.Printf("[shaper] → (%.1f↓ %.1f↑ Mbps, %s)", downMbps, upMbps, dur.Round(time.Millisecond))
	}
}

// shapedWriter wraps an io.Writer and throttles each Write through an
// atomically-updated rate bucket. When the bucket is superseded mid-sleep,
// it immediately retries with the new bucket.
type shapedWriter struct {
	w      io.Writer
	bucket *atomic.Pointer[rateBucket]
}

func (sw *shapedWriter) Write(p []byte) (int, error) {
	for {
		b := sw.bucket.Load()
		if b == nil || b.wait(int64(len(p))) {
			break
		}
		// bucket was superseded; loop to pick up the new one
	}
	return sw.w.Write(p)
}
