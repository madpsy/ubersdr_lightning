// spectrum.go — VLF spectrum analyser for ubersdr_lightning
//
// Accumulates IQ samples from the detector's receive loop, computes a
// Hann-windowed FFT every spectrumInterval seconds, and broadcasts the
// averaged power spectrum to SSE clients as a named "spectrum" event.
//
// The spectrum covers the full iq48 band:
//
//	centre = 25 kHz, bandwidth = 48 kHz → 1–49 kHz
//	FFT size = 4096 → bin width = 48000/4096 ≈ 11.7 Hz
//	Output = 2048 positive-frequency bins (DC to Nyquist)
//
// Frequency of bin k:
//
//	f(k) = (centreHz - sampleRate/2) + k * (sampleRate / fftSize)
//	     = 1000 + k * 11.71875 Hz
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
	fftSize          = 4096                    // must be power of 2
	fftBins          = fftSize / 2             // positive-frequency bins (2048)
	spectrumInterval = 5 * time.Second         // how often to broadcast
	spectrumMinDB    = -120.0                  // floor for display
	spectrumMaxDB    = 0.0                     // ceiling (0 dBFS)
)

// binFreqHz returns the centre frequency in Hz of FFT bin k.
// With centre=25000 Hz and sampleRate=48000:
//   f(0)    ≈ 1000 Hz  (lower edge)
//   f(2047) ≈ 48988 Hz (upper edge)
func binFreqHz(k int) float64 {
	return float64(iqCentreHz-iqSampleRate/2) + float64(k)*float64(iqSampleRate)/float64(fftSize)
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
	accumPower [fftBins]float64
	accumCount int // number of FFT frames accumulated

	// Latest averaged spectrum (dBFS per bin), protected by mu
	latest [fftBins]float32

	// Sample ring buffer for the current FFT frame
	ibuf [fftSize]float64 // I samples
	qbuf [fftSize]float64 // Q samples
	bufIdx int            // next write position in ring buffer

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

// processFrame applies the Hann window and computes the FFT power spectrum
// for the current buffer, accumulating into accumPower.
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

	// Accumulate power spectrum (positive frequencies only)
	// Normalise by fftSize and window power (Hann: sum(w²)/N ≈ 0.375)
	const hannPower = 0.375
	norm := float64(fftSize) * float64(fftSize) * hannPower
	for k := 0; k < fftBins; k++ {
		re := real(x[k])
		im := imag(x[k])
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
		Bins      string  `json:"bins"`      // base64(float32 LE × 2048)
		BinCount  int     `json:"bin_count"` // 2048
		FreqStart float64 `json:"freq_start_hz"` // Hz of bin 0
		FreqEnd   float64 `json:"freq_end_hz"`   // Hz of bin 2047
		BinWidth  float64 `json:"bin_width_hz"`  // Hz per bin
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

// Latest returns the most recent averaged spectrum as a slice of dBFS values.
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
