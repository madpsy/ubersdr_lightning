// lightning.go — VLF sferic detector for ubersdr_lightning
//
// Connects to UberSDR in iq48 mode (48 kHz IQ, centred at 25 kHz, covering
// roughly 1–49 kHz) and detects lightning sferics using:
//
//  1. Warm-up period: IIR noise floor settles for warmupSeconds before
//     the trigger is armed, preventing false triggers on connection.
//  2. Envelope detection: √(I²+Q²) per sample pair
//  3. Adaptive IIR noise floor: slow-tracking background level
//  4. Threshold trigger: envelope > noiseFloor × thresholdRatio
//  5. Shape validation: duration 0.5–10 ms, single-peak check
//  6. Waveform capture: pre+post trigger window for TDOA cross-correlation
//
// Detected strikes are stored in a thread-safe ring buffer and broadcast
// to SSE clients via the strikeHub channel.  The SSE payload omits the
// raw waveform (too large); waveforms are available via GET /api/strikes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	// IQ channel parameters
	iqMode       = "iq48" // 48 kHz IQ bandwidth
	iqCentreHz   = 25000  // 25 kHz centre → covers 1–49 kHz (lower edge safely above DC)
	iqSampleRate = 48000  // samples per second per channel (I or Q)

	// Warm-up: number of seconds to settle the IIR noise floor before arming
	// the trigger.  Prevents false triggers immediately after connection.
	warmupSeconds = 5

	// Saturation detection: when both I and Q rail at ±32767, the envelope
	// = √2 ≈ 1.4142. A peak above this threshold indicates ADC clipping.
	// Saturated events are flagged (not rejected) — a very close lightning
	// strike can genuinely saturate the ADC; the GPS timestamp is still valid
	// for TDOA even if the waveform amplitude is meaningless.
	saturationLimit = 0.99

	// Sferic detection parameters
	// IIR noise floor: α controls how fast the floor tracks background changes.
	// α = 0.9999 → time constant ≈ 1/(1-α)/sampleRate ≈ 2 s at 48 kHz.
	// The floor rises slowly so brief sferics don't inflate it.
	defaultIIRAlpha = 0.9999

	// Threshold: trigger when envelope > noiseFloor × ratio.
	// 4.0 = 12 dB above noise floor — conservative enough to reject most
	// 50/60 Hz interference transients while still catching real sferics.
	defaultThresholdRatio = 4.0

	// Sferic duration gates (samples at 48 kHz)
	minSfericSamples = 24  // 0.5 ms
	maxSfericSamples = 480 // 10 ms

	// Single-peak validation: the envelope must fall back below
	// peakAmplitude × peakDecayRatio before the end of the armed window.
	// This rejects multi-cycle interference (e.g. 50 Hz transients) that
	// would show multiple peaks above threshold.
	peakDecayRatio = 0.5

	// Waveform capture window: captureMs milliseconds each side of the peak
	captureMs      = 10 // milliseconds
	captureSamples = captureMs * iqSampleRate / 1000 // samples per side

	// Strike history ring buffer depth
	strikeHistoryDepth = 1000

	// Reconnect delay on WebSocket error
	reconnectDelay = 5 * time.Second

	// Keepalive ping interval
	keepaliveInterval = 30 * time.Second
)

// ---------------------------------------------------------------------------
// StrikeEvent — one detected sferic
// ---------------------------------------------------------------------------

// StrikeEvent represents a single detected lightning sferic.
type StrikeEvent struct {
	// ID is a unique identifier for this strike (UUID).
	ID string `json:"id"`

	// TimestampNs is the GPS-synchronised Unix timestamp in nanoseconds of the
	// peak envelope sample. This is the value used for TDOA cross-correlation
	// between stations.
	TimestampNs int64 `json:"timestamp_ns"`

	// TimestampUTC is the human-readable UTC time of the strike.
	TimestampUTC time.Time `json:"timestamp_utc"`

	// PeakAmplitude is the normalised peak envelope value [0, 1] at the trigger.
	PeakAmplitude float64 `json:"peak_amplitude"`

	// NoiseFloor is the IIR noise floor value at the time of detection.
	NoiseFloor float64 `json:"noise_floor"`

	// SNR is PeakAmplitude / NoiseFloor (linear ratio).
	SNR float64 `json:"snr"`

	// SNRdB is SNR expressed in decibels (20·log10(SNR)).
	SNRdB float64 `json:"snr_db"`

	// DurationSamples is the number of samples the envelope stayed above
	// the trigger threshold (sferic duration).
	DurationSamples int `json:"duration_samples"`

	// DurationMs is DurationSamples converted to milliseconds.
	DurationMs float64 `json:"duration_ms"`

	// Saturated is true when the peak envelope reached the ADC ceiling
	// (peak ≥ saturationLimit ≈ 0.99, i.e. envelope ≈ √2 ≈ 1.414).
	// A saturated event may be a very close lightning strike (ADC overloaded
	// by a genuine sferic) or local interference. The GPS timestamp is still
	// valid for TDOA even when the waveform amplitude is clipped.
	Saturated bool `json:"saturated"`

	// Waveform is a pre+post trigger window of normalised envelope values.
	// Length ≈ 2×captureSamples. Used for TDOA cross-correlation.
	// Omitted from SSE broadcasts (too large); available via /api/strikes.
	Waveform []float64 `json:"waveform,omitempty"`
}

// stripWaveform returns a copy of s with Waveform set to nil.
// Used for SSE broadcasts to keep payload small.
func (s StrikeEvent) stripWaveform() StrikeEvent {
	s.Waveform = nil
	return s
}

// ---------------------------------------------------------------------------
// StrikeHistory — thread-safe ring buffer
// ---------------------------------------------------------------------------

// StrikeHistory is a fixed-size ring buffer of the most recent StrikeEvents.
type StrikeHistory struct {
	mu    sync.RWMutex
	buf   [strikeHistoryDepth]StrikeEvent
	head  int // next write position
	count int // number of valid entries (0..strikeHistoryDepth)
}

// Add appends a strike to the ring buffer.
func (h *StrikeHistory) Add(s StrikeEvent) {
	h.mu.Lock()
	h.buf[h.head] = s
	h.head = (h.head + 1) % strikeHistoryDepth
	if h.count < strikeHistoryDepth {
		h.count++
	}
	h.mu.Unlock()
}

// Recent returns up to n most recent strikes in chronological order (oldest first).
func (h *StrikeHistory) Recent(n int) []StrikeEvent {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if n > h.count {
		n = h.count
	}
	if n == 0 {
		return nil
	}
	out := make([]StrikeEvent, n)
	// The most recent entry is at (head-1+depth)%depth; walk backwards.
	for i := 0; i < n; i++ {
		idx := (h.head - 1 - i + strikeHistoryDepth) % strikeHistoryDepth
		out[n-1-i] = h.buf[idx]
	}
	return out
}

// Count returns the total number of strikes stored.
func (h *StrikeHistory) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.count
}

// ---------------------------------------------------------------------------
// LightningDetector — IQ stream consumer and sferic detector
// ---------------------------------------------------------------------------

// DetectorConfig holds tunable parameters for the sferic detector.
type DetectorConfig struct {
	// UberSDR WebSocket URL (ws:// or wss://)
	UberSDRURL string

	// CentreHz is the IQ channel centre frequency in Hz (default: 25000).
	// At 25 kHz centre with iq48 (±24 kHz), the band covers 1–49 kHz —
	// safely above DC and spanning the full VLF sferic spectrum.
	CentreHz int

	// IIRAlpha controls the noise floor tracking speed (default: 0.9999).
	// Higher = slower tracking. Range: 0.99–0.99999.
	IIRAlpha float64

	// ThresholdRatio: trigger when envelope > noiseFloor × ratio (default: 4.0).
	ThresholdRatio float64
}

// LightningDetector connects to UberSDR in iq48 mode and detects sferics.
type LightningDetector struct {
	cfg     DetectorConfig
	history *StrikeHistory

	// strikeOut receives every detected StrikeEvent for broadcast to SSE clients.
	strikeOut chan StrikeEvent

	// specAnalyser receives raw IQ PCM bytes for spectrum analysis.
	specAnalyser *SpectrumAnalyser

	// sessionID is the active user_session_id for the WebSocket connection.
	sessionID string
}

// NewLightningDetector creates a LightningDetector with the given config.
func NewLightningDetector(cfg DetectorConfig, history *StrikeHistory, strikeOut chan StrikeEvent, specAnalyser *SpectrumAnalyser) *LightningDetector {
	if cfg.CentreHz == 0 {
		cfg.CentreHz = iqCentreHz
	}
	if cfg.IIRAlpha == 0 {
		cfg.IIRAlpha = defaultIIRAlpha
	}
	if cfg.ThresholdRatio == 0 {
		cfg.ThresholdRatio = defaultThresholdRatio
	}
	return &LightningDetector{
		cfg:          cfg,
		history:      history,
		strikeOut:    strikeOut,
		specAnalyser: specAnalyser,
	}
}

// Run starts the IQ receive loop. Blocks until ctx is cancelled.
func (ld *LightningDetector) Run(ctx context.Context) {
	log.Printf("[lightning] detector starting — centre=%d Hz, alpha=%.5f, threshold=×%.1f",
		ld.cfg.CentreHz, ld.cfg.IIRAlpha, ld.cfg.ThresholdRatio)

	dec, err := newPCMDecoder()
	if err != nil {
		log.Printf("[lightning] PCM decoder init failed: %v", err)
		return
	}
	defer dec.close()

	for {
		if ctx.Err() != nil {
			return
		}

		ld.sessionID = uuid.New().String()
		if err := ld.checkConnection(); err != nil {
			log.Printf("[lightning] connection check failed: %v — retrying in %s", err, reconnectDelay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectDelay):
			}
			continue
		}

		wsAddr := ld.buildWSURL()
		hdr := http.Header{}
		hdr.Set("User-Agent", "ubersdr_lightning/1.0")
		conn, _, err := wsDialer.Dial(wsAddr, hdr)
		if err != nil {
			log.Printf("[lightning] dial failed: %v — retrying in %s", err, reconnectDelay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectDelay):
			}
			continue
		}

		log.Printf("[lightning] IQ stream connected (%s)", wsAddr)

		connCtx, connCancel := context.WithCancel(ctx)

		// Keepalive pings
		go func() {
			ticker := time.NewTicker(keepaliveInterval)
			defer ticker.Stop()
			for {
				select {
				case <-connCtx.Done():
					return
				case <-ticker.C:
					conn.WriteJSON(map[string]string{"type": "ping"}) //nolint:errcheck
				}
			}
		}()

		// Read loop in its own goroutine
		readCh := make(chan iqReadResult, 8)
		var readChRecv <-chan iqReadResult = readCh
		go func() {
			for {
				mt, m, e := conn.ReadMessage()
				readCh <- iqReadResult{mt, m, e}
				if e != nil {
					return
				}
			}
		}()

		ld.runDetectionLoop(ctx, connCtx, connCancel, conn, dec, readChRecv)

		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

// iqReadResult is the message type used by runDetectionLoop's read goroutine.
type iqReadResult struct {
	msgType int
	msg     []byte
	err     error
}

// runDetectionLoop processes IQ packets from readCh and runs the sferic detector.
// Returns when the connection is lost or ctx is cancelled.
func (ld *LightningDetector) runDetectionLoop(
	ctx context.Context,
	connCtx context.Context,
	connCancel context.CancelFunc,
	conn *websocket.Conn,
	dec *pcmDecoder,
	readCh <-chan iqReadResult,
) {
	defer connCancel()
	defer conn.Close()

	// ---------------------------------------------------------------------------
	// Detector state
	// ---------------------------------------------------------------------------

	// IIR noise floor — initialised to a small positive value.
	// The warm-up period (warmupSeconds) lets this settle before arming.
	noiseFloor := 0.001

	// Pre-trigger ring buffer: holds the last captureSamples envelope values
	// so we can include the rising edge in the waveform capture.
	preTrig := make([]float64, captureSamples)
	preTrigIdx := 0

	// Pre-trigger timestamp ring buffer: GPS nanosecond timestamp for each
	// sample in preTrig (same indexing).
	preTrigTs := make([]int64, captureSamples)

	// Warm-up: count samples until the noise floor has settled.
	// warmupSamples = warmupSeconds × iqSampleRate
	const warmupSamples = warmupSeconds * iqSampleRate
	warmupRemaining := warmupSamples
	log.Printf("[lightning] warming up noise floor for %d s…", warmupSeconds)

	// Trigger state machine
	type trigState int
	const (
		stateIdle    trigState = iota
		stateArmed             // envelope crossed threshold — collecting sferic
		stateCapture           // collecting post-peak waveform
	)
	state := stateIdle

	var (
		trigPeak      float64   // peak envelope during armed state
		trigPeakTs    int64     // GPS timestamp of peak sample
		trigDuration  int       // samples above threshold
		trigPeakIdx   int       // index within armed window where peak occurred
		trigArmIdx    int       // sample index when trigger fired (for peak position)
		trigSaturated bool      // true if peak hit ADC ceiling (≥ saturationLimit)
		postCapBuf    []float64 // post-trigger envelope samples
		postCapLeft   int       // remaining post-trigger samples to collect
	)

	for {
		select {
		case <-ctx.Done():
			return

		case res := <-readCh:
			if res.err != nil {
				log.Printf("[lightning] read error: %v — reconnecting", res.err)
				return
			}
			if res.msgType != websocket.BinaryMessage {
				continue
			}

			pkt, err := dec.decode(res.msg, true)
			if err != nil || len(pkt.pcm) == 0 {
				continue
			}

			// Feed raw PCM to the spectrum analyser (non-blocking)
			if ld.specAnalyser != nil {
				ld.specAnalyser.AddSamples(pkt.pcm)
			}

			// Compute envelope for this packet
			env := envelope(pkt.pcm)
			nSamples := len(env)

			// Distribute the packet timestamp evenly across samples.
			// pkt.timestampNs is the GPS time of the first sample in the packet.
			const samplePeriodNs int64 = 1_000_000_000 / iqSampleRate
			baseTs := pkt.timestampNs

			// Process each sample through the detector state machine
			for i, e := range env {
				sampleTs := baseTs + int64(i)*samplePeriodNs

				// Update pre-trigger ring buffer (always, regardless of state)
				preTrig[preTrigIdx] = e
				preTrigTs[preTrigIdx] = sampleTs
				preTrigIdx = (preTrigIdx + 1) % captureSamples

				// Warm-up: update IIR but don't trigger
				if warmupRemaining > 0 {
					noiseFloor = ld.cfg.IIRAlpha*noiseFloor + (1-ld.cfg.IIRAlpha)*e
					warmupRemaining--
					if warmupRemaining == 0 {
						log.Printf("[lightning] warm-up complete — noise floor=%.6f, threshold=%.6f",
							noiseFloor, noiseFloor*ld.cfg.ThresholdRatio)
					}
					continue
				}

				// Update IIR noise floor only when idle (not during a trigger)
				if state == stateIdle {
					noiseFloor = ld.cfg.IIRAlpha*noiseFloor + (1-ld.cfg.IIRAlpha)*e
				}

				threshold := noiseFloor * ld.cfg.ThresholdRatio

				switch state {
				case stateIdle:
					if e > threshold {
						state = stateArmed
						trigPeak = e
						trigPeakTs = sampleTs
						trigDuration = 1
						trigPeakIdx = 0
						trigArmIdx = 0
					}

				case stateArmed:
					trigArmIdx++
					if e > threshold {
						trigDuration++
						if e > trigPeak {
							trigPeak = e
							trigPeakTs = sampleTs
							trigPeakIdx = trigArmIdx
						}
						// Guard against runaway triggers (continuous interference)
						if trigDuration > maxSfericSamples*3 {
							state = stateIdle
							trigDuration = 0
						}
					} else {
						// Envelope dropped below threshold — sferic ended.
						// Validate duration gate.
						if trigDuration < minSfericSamples || trigDuration > maxSfericSamples {
							state = stateIdle
							break
						}

						// Single-peak validation: the peak must have occurred in
						// the first half of the armed window (fast rise, slow decay).
						// This rejects multi-cycle interference where the peak is
						// near the end of the window.
						halfDur := trigDuration / 2
						if trigPeakIdx > halfDur {
							// Peak too late — likely a multi-cycle burst, not a sferic
							state = stateIdle
							break
						}

						// Saturation flag: if the peak hit the ADC ceiling
						// (envelope ≈ √2 ≈ 1.414), the waveform amplitude is clipped.
						// We still capture the event — a very close lightning strike
						// can genuinely saturate the ADC; the GPS timestamp remains
						// valid for TDOA. The Saturated flag lets the UI warn the user.
						trigSaturated = trigPeak >= saturationLimit

						// Start post-trigger capture
						state = stateCapture
						postCapBuf = make([]float64, 0, captureSamples)
						postCapLeft = captureSamples
					}

				case stateCapture:
					postCapBuf = append(postCapBuf, e)
					postCapLeft--
					if postCapLeft <= 0 || i == nSamples-1 {
						// Emit strike event
						strike := ld.buildStrike(
							trigPeakTs, trigPeak, noiseFloor, trigDuration,
							trigSaturated,
							preTrig, preTrigIdx, preTrigTs,
							postCapBuf,
						)
						ld.history.Add(strike)
						select {
						case ld.strikeOut <- strike:
						default:
							// SSE hub full — drop (non-blocking)
						}
						satTag := ""
						if strike.Saturated {
							satTag = " [SATURATED — ADC clipping]"
						}
						log.Printf("[lightning] strike detected: ts=%d peak=%.4f snr=%.1fdB dur=%.2fms%s",
							strike.TimestampNs, strike.PeakAmplitude, strike.SNRdB, strike.DurationMs, satTag)
						state = stateIdle
						trigDuration = 0
					}
				}
			}
		}
	}
}

// buildStrike assembles a StrikeEvent from detector state.
func (ld *LightningDetector) buildStrike(
	peakTs int64, peakAmp, noiseFloor float64, durationSamples int,
	saturated bool,
	preTrig []float64, preTrigIdx int, preTrigTs []int64,
	postCap []float64,
) StrikeEvent {
	// Reconstruct pre-trigger window in chronological order
	n := len(preTrig)
	preTrigOrdered := make([]float64, n)
	for i := 0; i < n; i++ {
		preTrigOrdered[i] = preTrig[(preTrigIdx+i)%n]
	}

	// Build waveform: pre-trigger + post-trigger
	waveform := make([]float64, 0, len(preTrigOrdered)+len(postCap))
	waveform = append(waveform, preTrigOrdered...)
	waveform = append(waveform, postCap...)

	snr := 0.0
	if noiseFloor > 0 {
		snr = peakAmp / noiseFloor
	}
	snrDB := 0.0
	if snr > 0 {
		snrDB = 20 * math.Log10(snr)
	}

	return StrikeEvent{
		ID:              uuid.New().String(),
		TimestampNs:     peakTs,
		TimestampUTC:    time.Unix(0, peakTs).UTC(),
		PeakAmplitude:   peakAmp,
		NoiseFloor:      noiseFloor,
		SNR:             snr,
		SNRdB:           snrDB,
		DurationSamples: durationSamples,
		DurationMs:      float64(durationSamples) * 1000.0 / iqSampleRate,
		Saturated:       saturated,
		Waveform:        waveform,
	}
}

// checkConnection registers a session with UberSDR's /connection endpoint.
func (ld *LightningDetector) checkConnection() error {
	base := httpBaseURL(ld.cfg.UberSDRURL)
	endpoint := base + "/connection"
	body := fmt.Sprintf(`{"user_session_id":"%s"}`, ld.sessionID)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ubersdr_lightning/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusForbidden:
		return fmt.Errorf("connection rejected (password required or IP banned)")
	case http.StatusServiceUnavailable:
		return fmt.Errorf("connection rejected (server full)")
	default:
		return fmt.Errorf("connection check returned HTTP %d", resp.StatusCode)
	}
}

// buildWSURL constructs the WebSocket URL for the iq48 audio stream.
func (ld *LightningDetector) buildWSURL() string {
	u, _ := url.Parse(ld.cfg.UberSDRURL)
	wsScheme := "ws"
	if u.Scheme == "https" || u.Scheme == "wss" {
		wsScheme = "wss"
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "" {
		path = "/ws"
	}
	q := url.Values{}
	q.Set("frequency", fmt.Sprintf("%d", ld.cfg.CentreHz))
	q.Set("mode", iqMode)
	q.Set("format", "pcm-zstd")
	q.Set("version", "2")
	q.Set("user_session_id", ld.sessionID)
	return fmt.Sprintf("%s://%s%s?%s", wsScheme, u.Host, path, q.Encode())
}

// httpBaseURL converts any UberSDR URL (ws/wss/http/https) to an HTTP base URL.
func httpBaseURL(rawURL string) string {
	u, _ := url.Parse(rawURL)
	scheme := u.Scheme
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "/ws" {
		path = ""
	}
	return fmt.Sprintf("%s://%s%s", scheme, u.Host, path)
}

// wsDialer is the gorilla WebSocket dialer shared across all connections.
var wsDialer = &websocket.Dialer{
	HandshakeTimeout: 10 * time.Second,
}

// ---------------------------------------------------------------------------
// SSE hub — fan-out of StrikeEvents to browser clients
// ---------------------------------------------------------------------------

// sseHub broadcasts StrikeEvents to all connected SSE clients.
type sseHub struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{clients: make(map[chan string]struct{})}
}

func (h *sseHub) subscribe() chan string {
	ch := make(chan string, 32)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sseHub) unsubscribe(ch chan string) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

// broadcast sends two SSE messages to all connected clients:
//
//  1. An unnamed message (onmessage) with strike metadata — no waveform.
//     Small (~150 bytes). Triggers the live flash, stats, and table update.
//
//  2. A named "waveform" event with {id, waveform}.
//     Larger (~7.5 KB). Populates the waveform gallery in the browser.
//     Clients listen with es.addEventListener('waveform', handler).
func (h *sseHub) broadcast(s StrikeEvent) {
	// Message 1: strike metadata (waveform stripped)
	metaData, err := json.Marshal(s.stripWaveform())
	if err != nil {
		return
	}
	metaMsg := fmt.Sprintf("data: %s\n\n", metaData)

	// Message 2: waveform payload (only when waveform is present)
	var wfMsg string
	if len(s.Waveform) > 0 {
		type wfPayload struct {
			ID       string    `json:"id"`
			Waveform []float64 `json:"waveform"`
		}
		wfData, wfErr := json.Marshal(wfPayload{ID: s.ID, Waveform: s.Waveform})
		if wfErr == nil {
			wfMsg = fmt.Sprintf("event: waveform\ndata: %s\n\n", wfData)
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- metaMsg:
		default:
			// Slow client — drop metadata frame
		}
		if wfMsg != "" {
			select {
			case ch <- wfMsg:
			default:
				// Slow client — drop waveform frame (non-fatal; gallery just won't update)
			}
		}
	}
}

// runBroadcaster reads from strikeOut and fans out to SSE clients.
func (h *sseHub) runBroadcaster(ctx context.Context, strikeOut <-chan StrikeEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-strikeOut:
			h.broadcast(s)
		}
	}
}
