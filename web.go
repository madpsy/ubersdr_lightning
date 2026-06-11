// web.go — HTTP server for ubersdr_lightning
//
// Endpoints:
//
//	GET  /                        → static/index.html (rendered as Go template with BasePath)
//	GET  /static/*                → embedded static files
//	GET  /api/strikes?n=N         → JSON array of recent strikes (includes waveforms)
//	GET  /api/spectrum            → JSON: latest FFT spectrum (4096 bins, dBFS, 1–49 kHz)
//	GET  /api/status              → JSON status (strike count, server time)
//	GET  /api/events              → SSE stream: full StrikeEvents + spectrum + waveform frames
//	GET  /api/events?minimal=1    → SSE stream: compact strike-only events, no spectrum/waveform
//
// Minimal SSE format (one unnamed message per strike):
//
//	data: {"time":"15:04:05.000","peak_amplitude":0.4231,"snr_db":14.3,"duration_ms":3.25,"noise_floor":0.00812,"saturated":false}
//
// When running behind UberSDR's addon proxy the proxy sets the
// X-Forwarded-Prefix header (e.g. "/addons/lightning").  index.html is
// rendered as a Go template and receives BasePath so all JS fetch() calls
// and the EventSource URL are correctly prefixed.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed static
var staticFiles embed.FS

// startHTTPServer starts the HTTP server and blocks until it returns an error.
func startHTTPServer(addr string, history *StrikeHistory, hub *sseHub, specAnalyser *SpectrumAnalyser) error {
	mux := http.NewServeMux()

	// Parse index.html as a Go template so BasePath can be injected.
	indexTmpl, indexTmplErr := func() (*template.Template, error) {
		data, err := staticFiles.ReadFile("static/index.html")
		if err != nil {
			return nil, err
		}
		return template.New("index").Parse(string(data))
	}()

	// basePath extracts the proxy prefix from the X-Forwarded-Prefix header.
	// UberSDR's addon proxy sets this when strip_prefix is true.
	// Returns "" when running standalone (direct access).
	basePath := func(r *http.Request) string {
		return strings.TrimRight(r.Header.Get("X-Forwarded-Prefix"), "/")
	}

	// Static files (embedded) — served at /static/*
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("embed sub: %w", err)
	}
	staticHandler := http.FileServer(http.FS(sub))
	mux.Handle("/static/", http.StripPrefix("/static/", staticHandler))

	// Root → index.html rendered with BasePath
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		if indexTmplErr != nil {
			http.Error(w, "template error: "+indexTmplErr.Error(), http.StatusInternalServerError)
			return
		}
		bp := basePath(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		indexTmpl.Execute(w, map[string]string{"BasePath": bp}) //nolint:errcheck
	})

	// GET /api/strikes[?n=N][&since=D][&minimal=1]
	//
	// ?n=N        — return at most N most recent strikes (default 100, max 1000)
	// ?since=D    — only return strikes within the past D (Go duration: 5m, 1h, 30s)
	//               Can be combined with ?n: the result is the intersection.
	// ?minimal=1  — strip the waveform field from each strike (~7.5 KB each).
	//               Use this for lightweight polling clients that don't need
	//               the raw waveform data for TDOA cross-correlation.
	//
	// Examples:
	//   /api/strikes                        → last 100 strikes (with waveforms)
	//   /api/strikes?n=50                   → last 50 strikes (with waveforms)
	//   /api/strikes?since=5m               → all strikes in the last 5 minutes
	//   /api/strikes?since=1h&n=200         → up to 200 strikes in the last hour
	//   /api/strikes?minimal=1              → last 100 strikes, no waveforms (~150 bytes each)
	//   /api/strikes?since=5m&minimal=1     → last 5 min, no waveforms
	mux.HandleFunc("/api/strikes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		n := 100
		if s := r.URL.Query().Get("n"); s != "" {
			if v, err := strconv.Atoi(s); err == nil && v > 0 {
				if v > 1000 {
					v = 1000
				}
				n = v
			}
		}

		// Optional time filter: ?since=5m / 1h / 30s / 2h30m
		var sinceNs int64
		if s := r.URL.Query().Get("since"); s != "" {
			d, err := time.ParseDuration(s)
			if err != nil {
				http.Error(w, "invalid since parameter (use Go duration: 5m, 1h, 30s)", http.StatusBadRequest)
				return
			}
			sinceNs = time.Now().Add(-d).UnixNano()
		}

		minimal := r.URL.Query().Get("minimal") == "1"

		strikes := history.Recent(n)
		if strikes == nil {
			strikes = []StrikeEvent{}
		}

		// Apply time filter if requested
		if sinceNs > 0 {
			filtered := strikes[:0]
			for _, s := range strikes {
				if s.TimestampNs >= sinceNs {
					filtered = append(filtered, s)
				}
			}
			strikes = filtered
		}

		// Strip waveforms if minimal mode requested
		if minimal {
			stripped := make([]StrikeEvent, len(strikes))
			for i, s := range strikes {
				stripped[i] = s.stripWaveform()
			}
			strikes = stripped
		}

		jsonResponse(w, strikes)
	})

	// GET /api/spectrum — latest FFT spectrum as JSON (for polling clients)
	// Returns: {bins:[dBFS...], bin_count, freq_start_hz, freq_end_hz, bin_width_hz}
	mux.HandleFunc("/api/spectrum", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		bins := specAnalyser.Latest()
		type specResp struct {
			Bins      []float32 `json:"bins"`
			BinCount  int       `json:"bin_count"`
			FreqStart float64   `json:"freq_start_hz"`
			FreqEnd   float64   `json:"freq_end_hz"`
			BinWidth  float64   `json:"bin_width_hz"`
		}
		jsonResponse(w, specResp{
			Bins:      bins,
			BinCount:  len(bins),
			FreqStart: binFreqHz(0),
			FreqEnd:   binFreqHz(len(bins) - 1),
			BinWidth:  float64(iqSampleRate) / float64(fftSize),
		})
	})

	// GET /api/status
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jsonResponse(w, map[string]interface{}{
			"strike_count": history.Count(),
			"server_time":  time.Now().UTC().Format(time.RFC3339Nano),
		})
	})

	// GET /api/events[?minimal=1] — SSE stream of live StrikeEvents
	//
	// Without ?minimal=1 (default / web UI):
	//   - unnamed message: full StrikeEvent JSON (no waveform)
	//   - event: waveform — {id, waveform} for gallery
	//   - event: spectrum — FFT spectrum frame every 5 s
	//   - event: connected / heartbeat
	//
	// With ?minimal=1 (external clients, scripts, IoT):
	//   - unnamed message only: compact JSON with time, peak_amplitude,
	//     snr_db, duration_ms, noise_floor, saturated
	//   - spectrum and waveform events are suppressed
	//   - heartbeat is still sent every 15 s
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "SSE not supported", http.StatusInternalServerError)
			return
		}

		minimal := r.URL.Query().Get("minimal") == "1"

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// Send an immediate named "connected" event so the client knows the
		// SSE connection is live even before any strikes arrive.
		fmt.Fprint(w, "event: connected\ndata: {}\n\n")
		flusher.Flush()

		ch := hub.subscribe()
		defer hub.unsubscribe(ch)

		// Heartbeat every 15 s to keep the connection alive through proxies
		// that close idle connections.
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		mode := "full"
		if minimal {
			mode = "minimal"
		}
		log.Printf("[sse] client connected (%s): %s", mode, r.RemoteAddr)
		defer log.Printf("[sse] client disconnected: %s", r.RemoteAddr)

		for {
			select {
			case <-r.Context().Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				if minimal {
					// In minimal mode: only forward unnamed strike messages.
					// Skip event: spectrum, event: waveform, event: connected.
					// Unnamed SSE messages start with "data: " (no "event:" line).
					if !isUnnamedSSEMessage(msg) {
						continue
					}
					// Reformat to compact minimal struct
					msg = toMinimalSSE(msg)
					if msg == "" {
						continue
					}
				}
				fmt.Fprint(w, msg)
				flusher.Flush()
			case <-ticker.C:
				fmt.Fprint(w, "event: heartbeat\ndata: {}\n\n")
				flusher.Flush()
			}
		}
	})

	log.Printf("[web] listening on %s", addr)
	return http.ListenAndServe(addr, mux)
}

// isUnnamedSSEMessage returns true if msg is an unnamed SSE message
// (i.e. starts with "data: " rather than "event: ...").
// Named events (spectrum, waveform, connected, heartbeat) start with "event:".
func isUnnamedSSEMessage(msg string) bool {
	return len(msg) > 6 && msg[:6] == "data: "
}

// toMinimalSSE parses a full StrikeEvent SSE message and returns a compact
// minimal SSE message containing only the fields useful for external clients.
// Returns "" if the message cannot be parsed.
func toMinimalSSE(msg string) string {
	// Strip the "data: " prefix and trailing "\n\n"
	const prefix = "data: "
	if len(msg) < len(prefix) {
		return ""
	}
	jsonStr := strings.TrimRight(msg[len(prefix):], "\n")

	var s StrikeEvent
	if err := json.Unmarshal([]byte(jsonStr), &s); err != nil {
		return ""
	}

	type minimalStrike struct {
		Time          string  `json:"time"`           // HH:MM:SS.mmm UTC
		PeakAmplitude float64 `json:"peak_amplitude"`
		SNRdB         float64 `json:"snr_db"`
		DurationMs    float64 `json:"duration_ms"`
		NoiseFloor    float64 `json:"noise_floor"`
		Saturated     bool    `json:"saturated"`
	}

	t := time.Unix(0, s.TimestampNs).UTC()
	m := minimalStrike{
		Time:          fmt.Sprintf("%02d:%02d:%02d.%03d", t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/1_000_000),
		PeakAmplitude: s.PeakAmplitude,
		SNRdB:         s.SNRdB,
		DurationMs:    s.DurationMs,
		NoiseFloor:    s.NoiseFloor,
		Saturated:     s.Saturated,
	}

	data, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("data: %s\n\n", data)
}

// jsonResponse writes v as JSON with Content-Type application/json.
func jsonResponse(w http.ResponseWriter, v interface{}) {
	data, err := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err != nil {
		_, _ = w.Write([]byte("[]\n"))
		return
	}
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n"))
}
