// spectrum.go — VLF spectrum analyser for ubersdr_lightning
//
// Accumulates IQ samples from the detector's receive loop, computes a
// Hann-windowed FFT every spectrumInterval seconds, and broadcasts the
// averaged power spectrum to SSE clients as a named "spectrum" event.
//
// The spectrum covers the full iq48 band using FFT-shift ordering:
//
//	centre = 25 kHz, bandwidth = 48 kHz → 1–49 kHz
//	FFT size = 4096 → bin width = 48000/4096 ≈ 11.7 Hz
//	Output = 4096 bins (full band, FFT-shifted so bin 0 = lowest frequency)
//
// FFT-shift maps raw FFT output to ascending frequency order:
//
//	raw bin 2048 → shifted bin 0    → 1000 Hz  (lower edge)
//	raw bin 4095 → shifted bin 2047 → 24988 Hz
//	raw bin 0    → shifted bin 2048 → 25000 Hz (centre)
//	raw bin 2047 → shifted bin 4095 → 48988 Hz (upper edge)
package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/cmplx"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	fftSize          = 4096            // must be power of 2
	fftBins          = fftSize         // all bins after FFT-shift (full 48 kHz)
	spectrumInterval = 5 * time.Second // how often to broadcast
	spectrumMinDB    = -120.0          // floor for display
	spectrumMaxDB    = 0.0             // ceiling (0 dBFS)
)

// binFreqHz returns the centre frequency in Hz of FFT-shifted bin k.
//
// After FFT-shift, bin ordering is:
//
//	k=0       → 1000 Hz  (centre - sampleRate/2, lower edge)
//	k=2047    → 24988 Hz (just below centre)
//	k=2048    → 25000 Hz (centre frequency)
//	k=4095    → 48988 Hz (upper edge)
func binFreqHz(k int) float64 {
	// Map shifted bin k back to raw FFT bin index, then to frequency.
	// Shifted bin k corresponds to raw bin (k + fftSize/2) % fftSize.
	rawBin := (k + fftSize/2) % fftSize
	// Raw bin 0 = DC = centre frequency; positive bins = above centre.
	// Frequency = centre + rawBin * binWidth  (for rawBin 0..N/2-1)
	// Frequency = centre + (rawBin - N) * binWidth (for rawBin N/2..N-1, negative freqs)
	binWidth := float64(iqSampleRate) / float64(fftSize)
	if rawBin < fftSize/2 {
		return float64(iqCentreHz) + float64(rawBin)*binWidth
	}
	return float64(iqCentreHz) + float64(rawBin-fftSize)*binWidth
}

// ---------------------------------------------------------------------------
// SpectrumAnalyser
// ---------------------------------------------------------------------------

// SpectrumAnalyser accumulates IQ samples, computes averaged FFT spectra,
// and broadcasts them to the SSE hub.
type SpectrumAnalyser struct {
	mu sync.Mutex

	// Hann window coefficients (precomputed)
	window [fftSize]float64

	// Accumulator: sum of power spectra across frames in the current interval
	// Indexed in FFT-shifted order (bin 0 = lowest frequency).
	accumPower [fftBins]float64
	accumCount int // number of FFT frames accumulated

	// Latest averaged spectrum (dBFS per bin), protected by mu
	// Indexed in FFT-shifted order (bin 0 = lowest frequency).
	latest [fftBins]float32

	// Sample ring buffer for the current FFT frame
	ibuf   [fftSize]float64 // I samples
	qbuf   [fftSize]float64 // Q samples
	bufIdx int              // next write position in ring buffer

	// Ticker for periodic broadcast
	ticker *time.Ticker

	// SSE hub reference for broadcasting
	hub *sseHub
}

// NewSpectrumAnalyser creates and starts a SpectrumAnalyser.
func NewSpectrumAnalyser(hub *sseHub) *SpectrumAnalyser {
	sa := &SpectrumAnalyser{hub: hub}
	// Precompute Hann window
	for i := 0; i < fftSize; i++ {
		sa.window[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(fftSize-1)))
	}
	return sa
}

// Start begins the periodic spectrum broadcast goroutine.
func (sa *SpectrumAnalyser) Start() {
	sa.ticker = time.NewTicker(spectrumInterval)
	go func() {
		for range sa.ticker.C {
			sa.flush()
		}
	}()
}

// Stop halts the periodic broadcast.
func (sa *SpectrumAnalyser) Stop() {
	if sa.ticker != nil {
		sa.ticker.Stop()
	}
}

// AddSamples feeds interleaved S16LE IQ PCM bytes into the analyser.
// Called from the detector's receive loop with each decoded packet.
func (sa *SpectrumAnalyser) AddSamples(pcm []byte) {
	if len(pcm) < 4 {
		return
	}
	sa.mu.Lock()
	defer sa.mu.Unlock()

	nSamples := len(pcm) / 4 // 2 bytes I + 2 bytes Q per sample pair
	for i := 0; i < nSamples; i++ {
		iRaw := int16(binary.LittleEndian.Uint16(pcm[i*4:]))
		qRaw := int16(binary.LittleEndian.Uint16(pcm[i*4+2:]))
		sa.ibuf[sa.bufIdx] = float64(iRaw) / 32768.0
		sa.qbuf[sa.bufIdx] = float64(qRaw) / 32768.0
		sa.bufIdx = (sa.bufIdx + 1) % fftSize

		// When we've filled a complete FFT frame, process it
		if sa.bufIdx == 0 {
			sa.processFrame()
		}
	}
}

// processFrame applies the Hann window, computes the FFT, and accumulates
// the power spectrum in FFT-shifted order (bin 0 = lowest frequency).
// Must be called with sa.mu held.
func (sa *SpectrumAnalyser) processFrame() {
	// Build complex input with Hann window
	x := make([]complex128, fftSize)
	for i := 0; i < fftSize; i++ {
		w := sa.window[i]
		x[i] = complex(sa.ibuf[i]*w, sa.qbuf[i]*w)
	}

	// In-place Cooley-Tukey FFT
	fft(x)

	// Accumulate power spectrum in FFT-shifted order.
	// FFT-shift: raw bin (fftSize/2 + k) % fftSize → shifted bin k
	// This maps negative frequencies first, then positive, giving
	// ascending frequency order from lower edge to upper edge.
	//
	// Normalise by fftSize² and Hann window power (≈ 0.375).
	const hannPower = 0.375
	norm := float64(fftSize) * float64(fftSize) * hannPower

	half := fftSize / 2
	for k := 0; k < fftSize; k++ {
		// Raw FFT bin for shifted position k
		rawBin := (k + half) % fftSize
		re := real(x[rawBin])
		im := imag(x[rawBin])
		sa.accumPower[k] += (re*re + im*im) / norm
	}
	sa.accumCount++
}

// flush averages the accumulated power spectra, converts to dBFS,
// stores as the latest spectrum, and broadcasts to SSE clients.
func (sa *SpectrumAnalyser) flush() {
	sa.mu.Lock()
	if sa.accumCount == 0 {
		sa.mu.Unlock()
		return
	}

	// Average and convert to dBFS
	var bins [fftBins]float32
	for k := 0; k < fftBins; k++ {
		avg := sa.accumPower[k] / float64(sa.accumCount)
		db := 10 * math.Log10(avg+1e-20) // +1e-20 avoids log(0)
		if db < spectrumMinDB {
			db = spectrumMinDB
		}
		if db > spectrumMaxDB {
			db = spectrumMaxDB
		}
		bins[k] = float32(db)
		sa.latest[k] = float32(db)
	}

	// Reset accumulator
	sa.accumPower = [fftBins]float64{}
	sa.accumCount = 0
	sa.mu.Unlock()

	// Encode as base64(float32 LE array) for compact SSE transport
	raw := make([]byte, fftBins*4)
	for k, v := range bins {
		binary.LittleEndian.PutUint32(raw[k*4:], math.Float32bits(v))
	}
	b64 := base64.StdEncoding.EncodeToString(raw)

	type specPayload struct {
		Bins      string  `json:"bins"`          // base64(float32 LE × 4096)
		BinCount  int     `json:"bin_count"`     // 4096
		FreqStart float64 `json:"freq_start_hz"` // Hz of bin 0 (≈ 1000 Hz)
		FreqEnd   float64 `json:"freq_end_hz"`   // Hz of bin 4095 (≈ 48988 Hz)
		BinWidth  float64 `json:"bin_width_hz"`  // Hz per bin (≈ 11.72 Hz)
	}
	payload, _ := json.Marshal(specPayload{
		Bins:      b64,
		BinCount:  fftBins,
		FreqStart: binFreqHz(0),
		FreqEnd:   binFreqHz(fftBins - 1),
		BinWidth:  float64(iqSampleRate) / float64(fftSize),
	})

	msg := fmt.Sprintf("event: spectrum\ndata: %s\n\n", payload)
	sa.hub.mu.Lock()
	for ch := range sa.hub.clients {
		select {
		case ch <- msg:
		default:
			// Slow client — drop spectrum frame
		}
	}
	sa.hub.mu.Unlock()
}

// Latest returns the most recent averaged spectrum as a slice of dBFS values
// in FFT-shifted order (bin 0 = lowest frequency ≈ 1 kHz).
// Used by GET /api/spectrum.
func (sa *SpectrumAnalyser) Latest() []float32 {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	out := make([]float32, fftBins)
	copy(out, sa.latest[:])
	return out
}

// ---------------------------------------------------------------------------
// Cooley-Tukey in-place radix-2 DIT FFT
// ---------------------------------------------------------------------------

// fft computes the in-place DFT of x (length must be a power of 2).
func fft(x []complex128) {
	n := len(x)
	if n <= 1 {
		return
	}

	// Bit-reversal permutation
	j := 0
	for i := 1; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}

	// Cooley-Tukey butterfly
	for length := 2; length <= n; length <<= 1 {
		angle := -2 * math.Pi / float64(length)
		wlen := cmplx.Exp(complex(0, angle))
		for i := 0; i < n; i += length {
			w := complex(1, 0)
			half := length / 2
			for k := 0; k < half; k++ {
				u := x[i+k]
				v := x[i+k+half] * w
				x[i+k] = u + v
				x[i+k+half] = u - v
				w *= wlen
			}
		}
	}
}
