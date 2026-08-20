// candidates.go — trigger-candidate diagnostic log
//
// Every threshold crossing (envelope > noiseFloor × ratio) becomes a
// "candidate". Most candidates never become strikes: they are rejected by the
// duration gate, the peak-position gate, the runaway guard, the refractory
// period, or the rate limiter. Until now those rejections were silent, which
// made the detector impossible to diagnose ("I can see sferics on the
// spectrum — why no strikes?").
//
// CandidateLog records the outcome of every candidate in a fixed-size ring
// buffer (candidateLogDepth entries) plus lifetime per-reason counters, and
// is exposed via GET /api/candidates for the web UI.
package main

import (
	"sync"
	"time"
)

// candidateLogDepth is the ring buffer size for the candidate diagnostic log.
const candidateLogDepth = 250

// Candidate rejection reasons. reasonAccepted marks candidates that became
// real StrikeEvents.
const (
	reasonAccepted   = "accepted"
	reasonTooShort   = "too_short"    // duration below MinSfericMs
	reasonTooLong    = "too_long"     // duration above MaxSfericMs
	reasonRunaway    = "runaway"      // duration above runawayFactor × MaxSfericMs — continuous interference
	reasonPeakLate   = "peak_late"    // peak in second half of window — multi-cycle burst
	reasonRefractory = "refractory"   // within RefractoryMs of the last strike
	reasonRateLimit  = "rate_limited" // > MaxStrikesPerMin in the last 60 s
)

// CandidateEvent is one threshold crossing and its outcome.
type CandidateEvent struct {
	// Seq is a monotonically increasing sequence number (1 = first candidate
	// since process start). Stable across ring-buffer wraparound.
	Seq uint64 `json:"seq"`

	// TimestampNs is the GPS timestamp of the peak envelope sample.
	TimestampNs int64 `json:"timestamp_ns"`

	// TimestampUTC is the human-readable UTC time of the peak.
	TimestampUTC time.Time `json:"timestamp_utc"`

	// PeakAmplitude is the peak normalised envelope value during the crossing.
	PeakAmplitude float64 `json:"peak_amplitude"`

	// NoiseFloor and Threshold are the detector values at the time of the
	// crossing (Threshold = NoiseFloor × ThresholdRatio).
	NoiseFloor float64 `json:"noise_floor"`
	Threshold  float64 `json:"threshold"`

	// SNRdB is 20·log10(PeakAmplitude / NoiseFloor).
	SNRdB float64 `json:"snr_db"`

	// DurationSamples / DurationMs is how long the envelope stayed above
	// threshold before the candidate was resolved.
	DurationSamples int     `json:"duration_samples"`
	DurationMs      float64 `json:"duration_ms"`

	// Accepted is true when the candidate became a StrikeEvent.
	Accepted bool `json:"accepted"`

	// Reason is reasonAccepted or one of the rejection reasons above.
	Reason string `json:"reason"`
}

// CandidateLog is a thread-safe ring buffer of recent CandidateEvents plus
// lifetime per-reason totals (totals keep counting even after the ring wraps,
// so sustained rejection storms remain visible).
type CandidateLog struct {
	mu     sync.RWMutex
	buf    [candidateLogDepth]CandidateEvent
	head   int // next write position
	count  int // valid entries (0..candidateLogDepth)
	seq    uint64
	totals map[string]uint64
}

// NewCandidateLog creates an empty CandidateLog.
func NewCandidateLog() *CandidateLog {
	return &CandidateLog{totals: make(map[string]uint64)}
}

// Add records a resolved candidate.
func (l *CandidateLog) Add(c CandidateEvent) {
	l.mu.Lock()
	l.seq++
	c.Seq = l.seq
	l.buf[l.head] = c
	l.head = (l.head + 1) % candidateLogDepth
	if l.count < candidateLogDepth {
		l.count++
	}
	l.totals[c.Reason]++
	l.mu.Unlock()
}

// Page returns one page of candidates, newest first. page is 1-based.
// Also returns the number of entries currently in the ring buffer.
func (l *CandidateLog) Page(page, perPage int) ([]CandidateEvent, int) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	start := (page - 1) * perPage
	if start >= l.count {
		return []CandidateEvent{}, l.count
	}
	n := perPage
	if start+n > l.count {
		n = l.count - start
	}
	out := make([]CandidateEvent, n)
	// Newest entry is at (head-1+depth)%depth; walk backwards from there.
	for i := 0; i < n; i++ {
		idx := (l.head - 1 - start - i + 2*candidateLogDepth) % candidateLogDepth
		out[i] = l.buf[idx]
	}
	return out, l.count
}

// Totals returns a copy of the lifetime per-reason counters.
func (l *CandidateLog) Totals() map[string]uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]uint64, len(l.totals))
	for k, v := range l.totals {
		out[k] = v
	}
	return out
}
