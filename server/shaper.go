package main

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"log"
	"math"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Phase describes a traffic shaping profile: bandwidth limits and duration range.
type Phase struct {
	Name        string
	MinDuration time.Duration
	MaxDuration time.Duration
	DownMbps    float64 // server→client bandwidth cap
	UpMbps      float64 // client→server bandwidth cap (applied on the client side)
}

// Phases is the set of traffic profiles SmartShaper randomly cycles through.
// Profiles mirror realistic browser traffic patterns:
//
//	idle      — user is reading, no data transfer
//	page_load — loading HTML/CSS/JS/fonts of a new page
//	images    — loading image gallery or thumbnails
//	api_call  — short XHR/fetch request
//	upload    — user uploading a file or photo
var Phases = []Phase{
	{"idle", 1 * time.Second, 2 * time.Second, 0.0, 0.0},
	{"page_load", 1 * time.Second, 4 * time.Second, 12.0, 0.8},
	{"images", 1 * time.Second, 4 * time.Second, 6.0, 0.1},
	{"api_call", 500 * time.Millisecond, 2 * time.Second, 0.4, 0.3},
	{"upload", 1 * time.Second, 5 * time.Second, 0.3, 4.0},
}

// randPhaseIdx returns a random phase index different from last.
// last == -1 means no constraint (first selection).
func randPhaseIdx(last int) int {
	for {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(Phases))))
		if err != nil {
			// crypto/rand failure — fall back to simple round-robin
			return (last + 1) % len(Phases)
		}
		if idx := int(n.Int64()); idx != last {
			return idx
		}
	}
}

// randPhaseDuration returns a random duration in [p.MinDuration, p.MaxDuration).
func randPhaseDuration(p Phase) time.Duration {
	span := p.MaxDuration - p.MinDuration
	if span <= 0 {
		return p.MinDuration
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(span)))
	if err != nil {
		return p.MinDuration
	}
	return p.MinDuration + time.Duration(n.Int64())
}

// randGapDuration returns a random inter-phase gap in [1, 30) seconds.
func randGapDuration() time.Duration {
	const minGap = 1 * time.Second
	const maxGap = 30 * time.Second
	n, err := rand.Int(rand.Reader, big.NewInt(int64(maxGap-minGap)))
	if err != nil {
		return minGap
	}
	return minGap + time.Duration(n.Int64())
}

// rateBucket is a token-bucket rate limiter with a cancellable sleep.
// When paused (bytesPerSec == 0), writes block entirely until the phase changes.
type rateBucket struct {
	paused   bool
	mu       sync.Mutex
	tokens   float64
	rate     float64 // bytes per nanosecond
	capacity float64
	last     time.Time
	done     chan struct{} // closed when this bucket is superseded by a phase change
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
// Returns false if the bucket was superseded (phase changed) — caller retries with new bucket.
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

// sessionShaper manages the download rate limit for one client session.
// The server throttles server→client traffic; the client independently throttles
// client→server traffic using the same phase index it receives.
type sessionShaper struct {
	down atomic.Pointer[rateBucket]
}

// setPhase replaces the download bucket and wakes any goroutine sleeping in the old one.
func (s *sessionShaper) setPhase(p Phase) {
	if old := s.down.Swap(newRateBucket(p.DownMbps * 125_000)); old != nil {
		close(old.done)
	}
}

// runPhaseEngine randomly cycles phases (never repeating the previous one),
// notifies the client via controlStream with full phase parameters,
// and updates local download rate limits. Blocks until the stream is closed.
//
// Protocol: each phase is announced as a 12-byte message:
//
//	[0:4]  duration_ms  uint32, big-endian
//	[4:8]  down_mbps    float32 bits, big-endian (IEEE 754)
//	[8:12] up_mbps      float32 bits, big-endian (IEEE 754)
func (s *sessionShaper) runPhaseEngine(controlStream net.Conn) {
	defer controlStream.Close()
	defer func() {
		if b := s.down.Swap(nil); b != nil {
			close(b.done)
		}
	}()
	lastIdx := -1
	for {
		idx := randPhaseIdx(lastIdx)
		p := Phases[idx]
		dur := randPhaseDuration(p)

		var msg [12]byte
		binary.BigEndian.PutUint32(msg[0:4], uint32(dur.Milliseconds()))
		binary.BigEndian.PutUint32(msg[4:8], math.Float32bits(float32(p.DownMbps)))
		binary.BigEndian.PutUint32(msg[8:12], math.Float32bits(float32(p.UpMbps)))

		controlStream.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := controlStream.Write(msg[:]); err != nil {
			return
		}
		controlStream.SetWriteDeadline(time.Time{})

		s.setPhase(p)
		log.Printf("[shaper] → %s (%.1f↓ %.1f↑ Mbps, %s)", p.Name, p.DownMbps, p.UpMbps, dur.Round(time.Millisecond))
		time.Sleep(dur)

		// Inter-phase gap: reset to no-throttle, wait 1–30 s before next phase.
		if old := s.down.Swap(nil); old != nil {
			close(old.done)
		}
		gap := randGapDuration()
		log.Printf("[shaper] gap %s before next phase", gap.Round(time.Millisecond))
		time.Sleep(gap)
		lastIdx = idx
	}
}

// shapedWriter wraps an io.Writer and throttles each Write through an
// atomically-updated rate bucket. When the bucket is superseded (phase change)
// mid-sleep, it immediately retries with the new bucket.
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

// downWriter wraps w with a download-rate limiter driven by s's current phase.
func (s *sessionShaper) downWriter(w io.Writer) io.Writer {
	return &shapedWriter{w: w, bucket: &s.down}
}
