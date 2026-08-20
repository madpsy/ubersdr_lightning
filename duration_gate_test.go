package main

import "testing"

func TestMsToSamples(t *testing.T) {
	cases := []struct {
		ms   float64
		want int
	}{
		{0.02, 1},   // the default minimum — must round UP to 1, never 0
		{10.0, 480}, // the default maximum
		{1.0, 48},   // the old minimum, for reference
		{0.0, 1},    // clamped: a zero-sample gate would be meaningless
		{-5.0, 1},   // clamped
		{0.0625, 3}, // 3 samples
	}
	for _, c := range cases {
		if got := msToSamples(c.ms); got != c.want {
			t.Errorf("msToSamples(%v) = %d, want %d", c.ms, got, c.want)
		}
	}
}

// TestDefaultGateAcceptsImpulseWidths guards the fix for the bug where every
// real sferic was rejected as "too short".
//
// A sferic is impulsive, so through a 48 kHz channel its envelope is only
// ~1/48 kHz ≈ 21 µs wide. The durations below are the ones actually observed
// in the field (candidate log, 2026-08-20 17:08–17:12, SNR 20–29 dB): 1 to 4
// samples. The previous 48-sample minimum rejected all of them.
func TestDefaultGateAcceptsImpulseWidths(t *testing.T) {
	cfg := DetectorConfig{UberSDRURL: "ws://x/ws"}
	det := NewLightningDetector(cfg, &StrikeHistory{}, nil, nil, nil)

	minSamples := msToSamples(det.cfg.MinSfericMs)
	maxSamples := msToSamples(det.cfg.MaxSfericMs)

	observed := []int{1, 1, 1, 1, 1, 4, 2, 3, 1, 4, 1}
	for _, d := range observed {
		if d < minSamples || d > maxSamples {
			t.Errorf("observed sferic of %d samples (%.3f ms) rejected by default gate %d–%d samples",
				d, float64(d)*1000/iqSampleRate, minSamples, maxSamples)
		}
	}

	// The old hardcoded minimum must not creep back in.
	if minSamples >= 48 {
		t.Errorf("minimum gate is %d samples — a 48 kHz channel cannot produce an impulse that wide", minSamples)
	}

	// The maximum must still reject sustained interference.
	if sustained := msToSamples(50); sustained <= maxSamples {
		t.Errorf("50 ms burst (%d samples) not rejected by max gate %d", sustained, maxSamples)
	}
}

// TestPeakCheckDefaultsEnabled pins the zero-value semantics of the inverted
// flag: an unset DisablePeakCheck must preserve the original behaviour.
func TestPeakCheckDefaultsEnabled(t *testing.T) {
	det := NewLightningDetector(DetectorConfig{}, &StrikeHistory{}, nil, nil, nil)
	if det.cfg.DisablePeakCheck {
		t.Error("peak check disabled by default; zero value should keep it enabled")
	}
}
