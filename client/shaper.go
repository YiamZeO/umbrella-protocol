package main

import (
	"crypto/rand"
	"io"
	"log"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// Phase describes a traffic shaping profile: bandwidth limits and duration range.
type Phase struct {
	Name        string
	MinDuration time.Duration
	MaxDuration time.Duration
	DownMbps    float64 // server→client bandwidth cap (applied on the client side)
	UpMbps      float64 // client→server bandwidth cap (applied on the client side)
}

var Phases []Phase

// loadPhases reads phases from YAML config file and populates the global Phases slice.
// YAML uses map format with phase name as key.
func loadPhases(filename string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}

	type phaseConfig struct {
		MinDuration int     `yaml:"min_duration"`
		MaxDuration int     `yaml:"max_duration"`
		DownMbps    float64 `yaml:"down_mbps"`
		UpMbps      float64 `yaml:"up_mbps"`
	}

	var config map[string]phaseConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return err
	}

	Phases = make([]Phase, 0, len(config))
	for name, p := range config {
		Phases = append(Phases, Phase{
			Name:        name,
			MinDuration: time.Duration(p.MinDuration) * time.Second,
			MaxDuration: time.Duration(p.MaxDuration) * time.Second,
			DownMbps:    p.DownMbps,
			UpMbps:      p.UpMbps,
		})
	}

	return nil
}

// randPhaseIdx returns a random phase index different from last (if phases more then 1).
// last == -1 means no constraint (first selection).
// Special case for len(Phases)==1 to avoid infinite loop.
func randPhaseIdx(last int) int {
	if len(Phases) == 0 {
		return 0
	}
	if len(Phases) == 1 {
		return 0
	}
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

// randGapDuration returns a random inter-phase gap in [1, 10) seconds.
func randGapDuration() time.Duration {
	const minGap = 1 * time.Second
	const maxGap = 10 * time.Second
	n, err := rand.Int(rand.Reader, big.NewInt(int64(maxGap-minGap)))
	if err != nil {
		return minGap
	}
	return minGap + time.Duration(n.Int64())
}

// gUpBucket is the current upload rate bucket, atomically replaced on each phase change.
// nil means no throttling (default/fallback behaviour).
var gUpBucket atomic.Pointer[rateBucket]

// gDownBucket is the current download rate bucket, atomically replaced on each phase change.
var gDownBucket atomic.Pointer[rateBucket]

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
		// Use a 100ms window for burst capacity to allow OS scheduler jitter
		// while maintaining steady average throughput.
		const burstWindow = 100 * time.Millisecond
		b.capacity = bytesPerSec * burstWindow.Seconds()

		// Ensure a minimum burst size (e.g., 64KB) to avoid excessive throttling
		// on low bandwidth limits where 100ms might be too small for typical MTU.
		if b.capacity < 64*1024 {
			b.capacity = 64 * 1024
		}

		b.tokens = b.capacity
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
	b.tokens -= float64(n)
	if b.tokens < 0 {
		sleep = time.Duration(-b.tokens / b.rate)
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

// refund adds back n tokens. Used when a Read returns fewer bytes than requested.
func (b *rateBucket) refund(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.tokens += float64(n)
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.mu.Unlock()
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

// applyPhase updates the global rate buckets with new limits and starts a local
// expiration timer for the phase. This is used by both the old server-driven and
// the new local engine.
func applyPhase(downMbps, upMbps float32, dur time.Duration, name string) {
	cancelCurrentPhase()

	newUpBucket := newRateBucket(float64(upMbps) * 125_000)
	if old := gUpBucket.Swap(newUpBucket); old != nil {
		close(old.done)
	}

	newDownBucket := newRateBucket(float64(downMbps) * 125_000)
	if old := gDownBucket.Swap(newDownBucket); old != nil {
		close(old.done)
	}

	cancelCh := make(chan struct{})
	phaseCancelMu.Lock()
	phaseCancel = cancelCh
	phaseCancelMu.Unlock()

	upBucket := newUpBucket     // capture for CAS
	downBucket := newDownBucket // capture for CAS
	go func() {
		select {
		case <-time.After(dur):
			// Phase expired: revert to no-throttle only if our bucket is still active.
			upExpired := gUpBucket.CompareAndSwap(upBucket, nil)
			if upExpired {
				close(upBucket.done)
			}
			downExpired := gDownBucket.CompareAndSwap(downBucket, nil)
			if downExpired {
				close(downBucket.done)
			}
			if upExpired || downExpired {
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
	log.Printf("[shaper] → %s (%.1f↓ %.1f↑ Mbps, %s)", name, downMbps, upMbps, dur.Round(time.Millisecond))
}

// runShaperEngine runs the phase selection loop locally on the client.
// No control stream to server is needed anymore. Phases are chosen randomly
// (avoiding consecutive repeats), applied for random duration, then a gap.
func runShaperEngine() {
	lastIdx := -1
	for {
		if len(Phases) == 0 {
			time.Sleep(10 * time.Second)
			continue
		}
		idx := randPhaseIdx(lastIdx)
		p := Phases[idx]
		dur := randPhaseDuration(p)

		applyPhase(float32(p.DownMbps), float32(p.UpMbps), dur, p.Name)

		time.Sleep(dur)

		// Inter-phase gap: reset to no-throttle, wait before next phase.
		gap := randGapDuration()
		log.Printf("[shaper] gap %s before next phase", gap.Round(time.Millisecond))
		time.Sleep(gap)
		lastIdx = idx
	}
}

// shapedWriter wraps an io.Writer and throttles each Write through an
// atomically-updated rate bucket. When the bucket is superseded mid-sleep,
// it immediately retries with the new bucket.
type shapedWriter struct {
	w      io.Writer
	bucket *atomic.Pointer[rateBucket]
}

func (sw *shapedWriter) Write(p []byte) (n int, err error) {
	var b *rateBucket
	for {
		b = sw.bucket.Load()
		if b == nil || b.wait(int64(len(p))) {
			break
		}
		// bucket was superseded; loop to pick up the new one
	}
	n, err = sw.w.Write(p)
	if b != nil && n < len(p) {
		b.refund(int64(len(p) - n))
	}
	return n, err
}

// shapedReader wraps an io.Reader and throttles each Read through an
// atomically-updated rate bucket.
// Throttle *before* Read to apply backpressure on the network stream
// (critical for effective download limiting).
type shapedReader struct {
	r      io.Reader
	bucket *atomic.Pointer[rateBucket]
}

func (sr *shapedReader) Read(p []byte) (n int, err error) {
	var b *rateBucket
	for {
		b = sr.bucket.Load()
		if b == nil || b.wait(int64(len(p))) {
			break
		}
	}
	n, err = sr.r.Read(p)
	if b != nil && n < len(p) {
		b.refund(int64(len(p) - n))
	}
	return n, err
}
