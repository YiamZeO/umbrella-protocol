package main

import (
	"crypto/rand"
	"encoding/binary"
	"log"
	"math"
	"math/big"
	"net"
	"os"
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

func init() {
	if err := loadPhases("phases.yml"); err != nil {
		log.Fatalf("failed to load phases from phases.yml: %v", err)
	}
	log.Printf("Loaded %d phases from phases.yml", len(Phases))
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

// runPhaseEngine randomly cycles phases (never repeating the previous one)
// and notifies the client via controlStream with full phase parameters.
// Blocks until the stream is closed.
//
// Protocol: each phase is announced as a 12-byte message:
//
//	[0:4]  duration_ms  uint32, big-endian
//	[4:8]  down_mbps    float32 bits, big-endian (IEEE 754)
//	[8:12] up_mbps      float32 bits, big-endian (IEEE 754)
func runPhaseEngine(controlStream net.Conn) {
	defer controlStream.Close()

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

		log.Printf("[shaper] → %s (%.1f↓ %.1f↑ Mbps, %s)", p.Name, p.DownMbps, p.UpMbps, dur.Round(time.Millisecond))
		time.Sleep(dur)

		// Inter-phase gap: reset to no-throttle, wait 1–30 s before next phase.
		// The client will automatically fall back to unthrottled when the phase duration expires locally.
		gap := randGapDuration()
		log.Printf("[shaper] gap %s before next phase", gap.Round(time.Millisecond))
		time.Sleep(gap)
		lastIdx = idx
	}
}
