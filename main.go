// main.go — ubersdr_lightning: VLF lightning sferic detector
//
// Connects to UberSDR in iq48 mode (48 kHz IQ, centred at 25 kHz, covering
// 1–49 kHz) and detects lightning sferics using an IIR adaptive noise floor
// and threshold trigger.
//
// Usage:
//
//	ubersdr_lightning -url ws://sdr.example.com/ws \
//	                  -listen :6097
//
// Environment variables (override flags):
//
//	UBERSDR_URL         — UberSDR WebSocket URL
//	WEB_PORT            — HTTP listen port (default 6097)
//	CENTRE_HZ           — IQ centre frequency in Hz (default 25000)
//	IIR_ALPHA           — IIR noise floor alpha (default 0.9999)
//	THRESHOLD_RATIO     — trigger threshold ratio (default 8.0)
//	REFRACTORY_MS       — dead time after each strike (default 100)
//	MAX_STRIKES_PER_MIN — rate limit before suppression (default 20)
//	MIN_SFERIC_MS       — minimum above-threshold duration (default 0.02)
//	MAX_SFERIC_MS       — maximum above-threshold duration (default 10)
//	PEAK_CHECK          — single-peak validation on/off (default true)
//	WARMUP_SECONDS      — noise floor settling time (default 5)
//	CAPTURE_MS          — waveform window each side of peak (default 10)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"strconv"
	"syscall"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloat64Or(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

// envBoolOr accepts the forms strconv.ParseBool understands — 1/0, t/f,
// true/false, and their capitalised variants. "yes"/"no" are not accepted and
// fall back to def.
func envBoolOr(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func main() {
	var (
		ubersdrURL       = flag.String("url", envOr("UBERSDR_URL", "ws://ubersdr:8080/ws"), "UberSDR WebSocket URL (env: UBERSDR_URL)")
		listenAddr       = flag.String("listen", ":"+envOr("WEB_PORT", "6097"), "HTTP listen address (env: WEB_PORT)")
		centreHz         = flag.Int("centre-hz", envIntOr("CENTRE_HZ", iqCentreHz), "IQ centre frequency in Hz (env: CENTRE_HZ)")
		iirAlpha         = flag.Float64("iir-alpha", envFloat64Or("IIR_ALPHA", defaultIIRAlpha), "IIR noise floor alpha 0.99–0.99999 (env: IIR_ALPHA)")
		thresholdRatio   = flag.Float64("threshold", envFloat64Or("THRESHOLD_RATIO", defaultThresholdRatio), "Trigger threshold ratio — 8.0 = 18 dB above noise floor (env: THRESHOLD_RATIO)")
		refractoryMs     = flag.Int("refractory-ms", envIntOr("REFRACTORY_MS", defaultRefractoryMs), "Refractory period in ms after each strike (env: REFRACTORY_MS)")
		maxStrikesPerMin = flag.Int("max-strikes-per-min", envIntOr("MAX_STRIKES_PER_MIN", defaultMaxStrikesPerMin), "Rate limit: max strikes per minute before suppression (env: MAX_STRIKES_PER_MIN)")
		minSfericMs      = flag.Float64("min-sferic-ms", envFloat64Or("MIN_SFERIC_MS", defaultMinSfericMs), "Minimum above-threshold duration in ms — 0.02 ≈ 1 sample (env: MIN_SFERIC_MS)")
		maxSfericMs      = flag.Float64("max-sferic-ms", envFloat64Or("MAX_SFERIC_MS", defaultMaxSfericMs), "Maximum above-threshold duration in ms (env: MAX_SFERIC_MS)")
		peakCheck        = flag.Bool("peak-check", envBoolOr("PEAK_CHECK", true), "Require the peak in the first half of the above-threshold window (env: PEAK_CHECK)")
		warmupSecs       = flag.Int("warmup-seconds", envIntOr("WARMUP_SECONDS", defaultWarmupSeconds), "Noise floor settling time before the trigger arms (env: WARMUP_SECONDS)")
		captureMs        = flag.Int("capture-ms", envIntOr("CAPTURE_MS", defaultCaptureMs), "Waveform capture window each side of the peak, in ms (env: CAPTURE_MS)")
	)
	flag.Parse()

	if *ubersdrURL == "" {
		fmt.Fprintln(os.Stderr, "error: -url (or UBERSDR_URL env) is required")
		flag.Usage()
		os.Exit(1)
	}

	log.Printf("[main] ubersdr_lightning starting")
	log.Printf("[main] UberSDR URL       : %s", *ubersdrURL)
	log.Printf("[main] Listen addr       : %s", *listenAddr)
	log.Printf("[main] Centre freq       : %d Hz", *centreHz)
	log.Printf("[main] IIR alpha         : %.5f", *iirAlpha)
	log.Printf("[main] Threshold ratio   : ×%.2f (%.1f dB)", *thresholdRatio, 20*math.Log10(*thresholdRatio))
	log.Printf("[main] Refractory period : %d ms", *refractoryMs)
	log.Printf("[main] Max strikes/min   : %d", *maxStrikesPerMin)
	log.Printf("[main] Sferic duration   : %.3f–%.3f ms (%d–%d samples)",
		*minSfericMs, *maxSfericMs, msToSamples(*minSfericMs), msToSamples(*maxSfericMs))
	log.Printf("[main] Peak-position gate: %v", *peakCheck)
	log.Printf("[main] Warm-up           : %d s", *warmupSecs)
	log.Printf("[main] Capture window    : ±%d ms", *captureMs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Shared strike history ring buffer
	history := &StrikeHistory{}

	// Trigger-candidate diagnostic log (250-entry ring, shown in the web UI)
	candidates := NewCandidateLog()

	// Channel from detector → SSE broadcaster
	strikeOut := make(chan StrikeEvent, 64)

	// SSE hub — fans out StrikeEvents and spectrum frames to browser clients
	hub := newSSEHub()

	// Spectrum analyser — computes FFT every 5 s and broadcasts via SSE
	specAnalyser := NewSpectrumAnalyser(hub, *centreHz)
	specAnalyser.Start()
	defer specAnalyser.Stop()

	// Lightning detector
	cfg := DetectorConfig{
		UberSDRURL:       *ubersdrURL,
		CentreHz:         *centreHz,
		IIRAlpha:         *iirAlpha,
		ThresholdRatio:   *thresholdRatio,
		RefractoryMs:     *refractoryMs,
		MaxStrikesPerMin: *maxStrikesPerMin,
		MinSfericMs:      *minSfericMs,
		MaxSfericMs:      *maxSfericMs,
		DisablePeakCheck: !*peakCheck,
		WarmupSeconds:    *warmupSecs,
		CaptureMs:        *captureMs,
	}
	det := NewLightningDetector(cfg, history, candidates, strikeOut, specAnalyser)

	// MQTT publishing through UberSDR's addon ingest port. Always on and needs
	// no configuration — the endpoint is derived from UBERSDR_URL. When the
	// receiver has MQTT disabled, or this container is not a recognised addon,
	// the publisher stays dormant and the detector is unaffected.
	//
	// It sits between the detector and the SSE hub, so the hub receives exactly
	// what it did before and neither component knows MQTT exists.
	mqttPub := NewMQTTPublisher(*ubersdrURL, history, det)
	hubIn := make(chan StrikeEvent, 64)
	go mqttPub.Run(ctx, strikeOut, hubIn)
	go hub.runBroadcaster(ctx, hubIn)

	go det.Run(ctx)

	// HTTP server (SSE + REST API + static UI)
	go func() {
		if err := startHTTPServer(*listenAddr, history, candidates, hub, specAnalyser); err != nil {
			log.Fatalf("[main] HTTP server: %v", err)
		}
	}()

	// Wait for SIGINT / SIGTERM
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Printf("[main] shutting down…")
	cancel()
	log.Printf("[main] done")
}
