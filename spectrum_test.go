package main

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// binFreqHz tests
// ---------------------------------------------------------------------------

// tolerance for floating-point comparisons (half a bin width at 48 kHz / 4096)
const binWidthHz = float64(iqSampleRate) / float64(fftSize) // ≈ 11.72 Hz
const freqTol = binWidthHz / 2                              // ≈ 5.86 Hz

func newSAWithCentre(centreHz int) *SpectrumAnalyser {
	// NewSpectrumAnalyser requires a *sseHub; pass nil — binFreqHz never
	// touches the hub, so this is safe for unit tests.
	return NewSpectrumAnalyser(nil, centreHz)
}

// approxEqual returns true when |a-b| ≤ tol.
func approxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

// TestBinFreqHz_DefaultCentre verifies the well-known anchor points at the
// default 25 kHz centre (iq48 mode, 48 kHz bandwidth).
func TestBinFreqHz_DefaultCentre(t *testing.T) {
	sa := newSAWithCentre(25000)

	cases := []struct {
		name   string
		bin    int
		wantHz float64
	}{
		// Lower edge: bin 0 → centre - sampleRate/2 = 25000 - 24000 = 1000 Hz
		{"lower_edge", 0, 1000.0},
		// Just below centre: bin fftSize/2 - 1 → 25000 - binWidth ≈ 24988.28 Hz
		{"just_below_centre", fftSize/2 - 1, 25000.0 - binWidthHz},
		// Centre bin: bin fftSize/2 → exactly 25000 Hz
		{"centre", fftSize / 2, 25000.0},
		// Upper edge: bin fftSize-1 → centre + sampleRate/2 - binWidth ≈ 48988.28 Hz
		{"upper_edge", fftSize - 1, 25000.0 + float64(iqSampleRate)/2.0 - binWidthHz},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sa.binFreqHz(tc.bin)
			if !approxEqual(got, tc.wantHz, freqTol) {
				t.Errorf("binFreqHz(%d) = %.4f Hz, want %.4f Hz (±%.4f)",
					tc.bin, got, tc.wantHz, freqTol)
			}
		})
	}
}

// TestBinFreqHz_CustomCentre_200kHz verifies that setting CENTRE_HZ=200000
// shifts all bin frequencies by exactly (200000 - 25000) = 175000 Hz relative
// to the default.  This is the exact scenario reported by Robert.
func TestBinFreqHz_CustomCentre_200kHz(t *testing.T) {
	const customCentre = 200_000
	const defaultCentre = 25_000
	const shift = float64(customCentre - defaultCentre) // 175000 Hz

	saDefault := newSAWithCentre(defaultCentre)
	saCustom := newSAWithCentre(customCentre)

	// Check every 256th bin to keep the test fast while covering the full range.
	for k := 0; k < fftSize; k += 256 {
		wantHz := saDefault.binFreqHz(k) + shift
		gotHz := saCustom.binFreqHz(k)
		if !approxEqual(gotHz, wantHz, freqTol) {
			t.Errorf("bin %d: binFreqHz with centre=%d Hz = %.4f Hz, want %.4f Hz (±%.4f)",
				k, customCentre, gotHz, wantHz, freqTol)
		}
	}
}

// TestBinFreqHz_CentreAnchor verifies that bin fftSize/2 always returns
// exactly the configured centre frequency, for several different centre values.
func TestBinFreqHz_CentreAnchor(t *testing.T) {
	centres := []int{
		10_000,  // 10 kHz
		25_000,  // default VLF
		77_500,  // mid-range
		200_000, // Robert's use-case
		500_000, // 500 kHz
	}
	centreBin := fftSize / 2

	for _, c := range centres {
		sa := newSAWithCentre(c)
		got := sa.binFreqHz(centreBin)
		if !approxEqual(got, float64(c), freqTol) {
			t.Errorf("centre=%d Hz: binFreqHz(%d) = %.4f Hz, want %.4f Hz",
				c, centreBin, got, float64(c))
		}
	}
}

// TestBinFreqHz_MonotonicallyIncreasing verifies that the FFT-shifted bin
// ordering produces strictly ascending frequencies across the full bin range.
func TestBinFreqHz_MonotonicallyIncreasing(t *testing.T) {
	sa := newSAWithCentre(200_000) // use Robert's custom centre

	prev := sa.binFreqHz(0)
	for k := 1; k < fftSize; k++ {
		cur := sa.binFreqHz(k)
		if cur <= prev {
			t.Errorf("non-monotonic at bin %d: freq[%d]=%.4f >= freq[%d]=%.4f",
				k, k, cur, k-1, prev)
		}
		prev = cur
	}
}

// TestBinFreqHz_BandwidthSymmetry verifies that the band is symmetric around
// the centre: lower edge is centre - sampleRate/2 and upper edge is
// centre + sampleRate/2 - binWidth.
func TestBinFreqHz_BandwidthSymmetry(t *testing.T) {
	centres := []int{25_000, 200_000}
	for _, c := range centres {
		sa := newSAWithCentre(c)
		lowerEdge := sa.binFreqHz(0)
		upperEdge := sa.binFreqHz(fftSize - 1)

		wantLower := float64(c) - float64(iqSampleRate)/2.0
		wantUpper := float64(c) + float64(iqSampleRate)/2.0 - binWidthHz

		if !approxEqual(lowerEdge, wantLower, freqTol) {
			t.Errorf("centre=%d: lower edge = %.4f Hz, want %.4f Hz", c, lowerEdge, wantLower)
		}
		if !approxEqual(upperEdge, wantUpper, freqTol) {
			t.Errorf("centre=%d: upper edge = %.4f Hz, want %.4f Hz", c, upperEdge, wantUpper)
		}
	}
}

// TestNewSpectrumAnalyser_ZeroCentreDefaultsToIqCentreHz verifies that
// passing centreHz=0 falls back to the package constant iqCentreHz (25000).
func TestNewSpectrumAnalyser_ZeroCentreDefaultsToIqCentreHz(t *testing.T) {
	sa := NewSpectrumAnalyser(nil, 0)
	got := sa.binFreqHz(fftSize / 2)
	if !approxEqual(got, float64(iqCentreHz), freqTol) {
		t.Errorf("zero centreHz: centre bin = %.4f Hz, want %.4f Hz (iqCentreHz=%d)",
			got, float64(iqCentreHz), iqCentreHz)
	}
}
