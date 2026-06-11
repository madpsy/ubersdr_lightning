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
//	UBERSDR_URL      — UberSDR WebSocket URL
//	WEB_PORT         — HTTP listen port (default 6097)
//	CENTRE_HZ        — IQ centre frequency in Hz (default 25000)
//	IIR_ALPHA        — IIR noise floor alpha (default 0.9999)
//	THRESHOLD_RATIO  — trigger threshold ratio (default 2.0)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
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

func main() {
	var (
		ubersdrURL     = flag.String("url", envOr("UBERSDR_URL", "ws://ubersdr:8080/ws"), "UberSDR WebSocket URL (env: UBERSDR_URL)")
		listenAddr     = flag.String("listen", ":"+envOr("WEB_PORT", "6097"), "HTTP listen address (env: WEB_PORT)")
		centreHz       = flag.Int("centre-hz", envIntOr("CENTRE_HZ", iqCentreHz), "IQ centre frequency in Hz (env: CENTRE_HZ)")
		iirAlpha       = flag.Float64("iir-alpha", envFloat64Or("IIR_ALPHA", defaultIIRAlpha), "IIR noise floor alpha 0.99–0.99999 (env: IIR_ALPHA)")
		thresholdRatio = flag.Float64("threshold", envFloat64Or("THRESHOLD_RATIO", defaultThresholdRatio), "Trigger threshold ratio (env: THRESHOLD_RATIO)")
	)
	flag.Parse()

	if *ubersdrURL == "" {
		fmt.Fprintln(os.Stderr, "error: -url (or UBERSDR_URL env) is required")
		flag.Usage()
		os.Exit(1)
	}

	log.Printf("[main] ubersdr_lightning starting")
	log.Printf("[main] UberSDR URL     : %s", *ubersdrURL)
	log.Printf("[main] Listen addr     : %s", *listenAddr)
	log.Printf("[main] Centre freq     : %d Hz", *centreHz)
	log.Printf("[main] IIR alpha       : %.5f", *iirAlpha)
	log.Printf("[main] Threshold ratio : ×%.2f", *thresholdRatio)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Shared strike history ring buffer
	history := &StrikeHistory{}

	// Channel from detector → SSE broadcaster
	strikeOut := make(chan StrikeEvent, 64)

	// SSE hub — fans out StrikeEvents and spectrum frames to browser clients
	hub := newSSEHub()
	go hub.runBroadcaster(ctx, strikeOut)

	// Spectrum analyser — computes FFT every 5 s and broadcasts via SSE
	specAnalyser := NewSpectrumAnalyser(hub)
	specAnalyser.Start()
	defer specAnalyser.Stop()

	// Lightning detector
	cfg := DetectorConfig{
		UberSDRURL:     *ubersdrURL,
		CentreHz:       *centreHz,
		IIRAlpha:       *iirAlpha,
		ThresholdRatio: *thresholdRatio,
	}
	det := NewLightningDetector(cfg, history, strikeOut, specAnalyser)
	go det.Run(ctx)

	// HTTP server (SSE + REST API + static UI)
	go func() {
		if err := startHTTPServer(*listenAddr, history, hub, specAnalyser); err != nil {
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
