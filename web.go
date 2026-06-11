// web.go — HTTP server for ubersdr_lightning
//
// Endpoints:
//
//	GET  /              → static/index.html (rendered as Go template with BasePath)
//	GET  /static/*      → embedded static files
//	GET  /api/strikes   → JSON array of recent strikes (query: ?n=100)
//	GET  /api/status    → JSON status (strike count, server time)
//	GET  /api/events    → SSE stream of live StrikeEvents
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
func startHTTPServer(addr string, history *StrikeHistory, hub *sseHub) error {
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

	// GET /api/strikes?n=100
	mux.HandleFunc("/api/strikes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		n := 100
		if s := r.URL.Query().Get("n"); s != "" {
			if v, err := strconv.Atoi(s); err == nil && v > 0 {
				n = v
			}
		}
		strikes := history.Recent(n)
		if strikes == nil {
			strikes = []StrikeEvent{}
		}
		jsonResponse(w, strikes)
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

	// GET /api/events — SSE stream of live StrikeEvents
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "SSE not supported", http.StatusInternalServerError)
			return
		}

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

		log.Printf("[sse] client connected: %s", r.RemoteAddr)
		defer log.Printf("[sse] client disconnected: %s", r.RemoteAddr)

		for {
			select {
			case <-r.Context().Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
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
