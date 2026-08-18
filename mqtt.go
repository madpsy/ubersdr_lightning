// mqtt.go — publishes strike events and detector status to UberSDR's MQTT feed.
//
// UberSDR exposes an ingest port on the sdr-network that lets addons publish
// through the receiver's own MQTT connection, and declare Home Assistant
// entities, without holding any broker credentials. See addon_mqtt.md in the
// ka9q_ubersdr repository.
//
// This is not optional and needs no configuration. The ingest endpoint is
// derived from UBERSDR_URL — same host, the ingest port — so the existing
// docker-compose.yml is sufficient as-is. If the receiver has MQTT turned off,
// or this container is not a recognised addon, publishing is simply skipped and
// the detector runs exactly as before. No publish failure is ever fatal.
//
// Two topics are published:
//
//	strikes   — one message per detected sferic (not retained)
//	summary   — rolling detector state, retained, refreshed every 30 s
//
// Home Assistant entities all read from the retained summary, so they show a
// sensible value the moment Home Assistant subscribes rather than waiting for
// the next strike.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// defaultIngestPort is UberSDR's mqtt.addon_ingest.port default. An operator
	// who changes it can point us at the new one with UBERSDR_INGEST_URL.
	defaultIngestPort = "6926"

	// addonVersion and addonModel populate this addon's Home Assistant device card.
	addonVersion = "1.1.0"
	addonModel   = "VLF lightning sferic detector"

	// summaryInterval is how often the retained summary is refreshed. It must be
	// comfortably below UberSDR's offline_after_sec (default 300 s), or the
	// receiver would mark us offline between updates and Home Assistant would
	// flap our entities.
	summaryInterval = 30 * time.Second

	// summaryWindow is the trailing window used for the strike-rate figure.
	summaryWindow = time.Hour

	ingestTimeout = 5 * time.Second
)

// ---------------------------------------------------------------------------
// Endpoint discovery
// ---------------------------------------------------------------------------

// ingestBaseURL works out where UberSDR's addon ingest port is.
//
// It is derived from UBERSDR_URL (which the container already has) by keeping
// the host and swapping in the ingest port — so a stock docker-compose.yml needs
// no new variables. UBERSDR_INGEST_URL overrides it outright for the rare case
// where the operator has moved the port.
func ingestBaseURL(ubersdrURL string) string {
	if v := strings.TrimRight(os.Getenv("UBERSDR_INGEST_URL"), "/"); v != "" {
		return v
	}

	u, err := url.Parse(ubersdrURL)
	if err != nil || u.Host == "" {
		// Fall back to the compose default service name rather than giving up:
		// on the sdr-network this is almost always right.
		return "http://ubersdr:" + defaultIngestPort
	}

	host := u.Hostname()
	if host == "" {
		return "http://ubersdr:" + defaultIngestPort
	}
	return "http://" + net.JoinHostPort(host, defaultIngestPort)
}

// ---------------------------------------------------------------------------
// Publisher
// ---------------------------------------------------------------------------

// MQTTPublisher pushes strike events and detector status to UberSDR.
// A nil *MQTTPublisher is usable — every method is a no-op — so callers never
// need to branch on availability.
type MQTTPublisher struct {
	base   string
	client *http.Client

	history *StrikeHistory
	det     *LightningDetector

	// available is set once the ingest port answers. When it never comes up we
	// keep quiet rather than logging a failure on every publish.
	available atomic.Bool

	// warned suppresses repeated "ingest unavailable" logging.
	warned atomic.Bool
}

// NewMQTTPublisher probes the ingest port and returns a publisher.
//
// It always returns a usable value: if the probe fails the publisher stays
// dormant and retries on the next summary tick, because MQTT may be enabled on
// the receiver after this addon has already started.
func NewMQTTPublisher(ubersdrURL string, history *StrikeHistory, det *LightningDetector) *MQTTPublisher {
	p := &MQTTPublisher{
		base:    ingestBaseURL(ubersdrURL),
		client:  &http.Client{Timeout: ingestTimeout},
		history: history,
		det:     det,
	}
	log.Printf("[mqtt] ingest endpoint: %s", p.base)
	p.probe()
	return p
}

// ingestHealth is the subset of GET /health this addon cares about.
type ingestHealth struct {
	Addon           string `json:"addon"`
	MQTTConnected   bool   `json:"mqtt_connected"`
	HADiscovery     bool   `json:"ha_discovery"`
	RateLimit       int    `json:"rate_limit"`
	OfflineAfterSec int    `json:"offline_after_sec"`
}

// probe checks whether the ingest port is reachable and we are recognised.
// Returns true when publishing is possible. Declares Home Assistant entities on
// the transition from unavailable to available, which covers both first start
// and the receiver enabling MQTT later.
func (p *MQTTPublisher) probe() bool {
	resp, err := p.client.Get(p.base + "/health")
	if err != nil {
		p.unavailable("ingest port unreachable (%v) — continuing without MQTT", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		p.unavailable("this container is not a recognised UberSDR addon — continuing without MQTT")
		return false
	}
	if resp.StatusCode != http.StatusOK {
		p.unavailable("ingest health returned %s — continuing without MQTT", resp.Status)
		return false
	}

	var h ingestHealth
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&h); err != nil {
		p.unavailable("could not parse ingest health (%v) — continuing without MQTT", err)
		return false
	}

	if p.available.Swap(true) {
		return true // already up; nothing new to announce
	}
	p.warned.Store(false)

	log.Printf("[mqtt] connected as addon %q (broker=%v, ha_discovery=%v, rate_limit=%d/min)",
		h.Addon, h.MQTTConnected, h.HADiscovery, h.RateLimit)

	if h.OfflineAfterSec > 0 && float64(h.OfflineAfterSec) < summaryInterval.Seconds()*2 {
		log.Printf("[mqtt] warning: receiver marks addons offline after %ds but we publish every %.0fs",
			h.OfflineAfterSec, summaryInterval.Seconds())
	}

	if h.HADiscovery {
		p.declareEntities()
	} else {
		log.Printf("[mqtt] Home Assistant discovery is disabled on the receiver — publishing data only")
	}
	return true
}

// unavailable records that publishing is off, logging only on a state change.
func (p *MQTTPublisher) unavailable(format string, args ...interface{}) {
	p.available.Store(false)
	if !p.warned.Swap(true) {
		log.Printf("[mqtt] "+format, args...)
	}
}

// ---------------------------------------------------------------------------
// Home Assistant entities
// ---------------------------------------------------------------------------

// haEntities are the entities this addon exposes. They all read from the single
// retained "summary" topic and are told apart by entity_key, which is why the
// addon publishes one compact payload rather than a topic per value.
func haEntities() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"sub_topic": "summary", "entity_key": "strike_rate",
			"component": "sensor", "name": "Strike Rate",
			"value_template":      "{{ value_json.strikes_last_hour }}",
			"unit_of_measurement": "strikes/h",
			"state_class":         "measurement",
			"icon":                "mdi:flash",
		},
		{
			"sub_topic": "summary", "entity_key": "strikes_total",
			"component": "sensor", "name": "Strikes Detected",
			"value_template":      "{{ value_json.strikes_total }}",
			"unit_of_measurement": "strikes",
			"state_class":         "total_increasing",
			"icon":                "mdi:counter",
		},
		// The three entities below describe the most recent strike, so their
		// fields are absent from the summary until one has been detected. Each
		// template renders an empty string in that case: Home Assistant skips a
		// state update on an empty render, leaving the entity "unknown" rather
		// than showing a fabricated zero. Piping an undefined value straight into
		// round() would instead raise a template error on every update.
		{
			"sub_topic": "summary", "entity_key": "last_snr",
			"component": "sensor", "name": "Last Strike SNR",
			"value_template":      "{{ value_json.last_snr_db | round(1) if value_json.last_snr_db is defined else '' }}",
			"unit_of_measurement": "dB",
			"state_class":         "measurement",
			"icon":                "mdi:signal-variant",
		},
		{
			"sub_topic": "summary", "entity_key": "noise_floor",
			"component": "sensor", "name": "Noise Floor",
			"value_template": "{{ value_json.noise_floor_db | round(1) if value_json.noise_floor_db is defined else '' }}",
			// Reported in dBFS relative to the ADC full scale.
			"unit_of_measurement": "dBFS",
			"state_class":         "measurement",
			"icon":                "mdi:waveform",
			"entity_category":     "diagnostic",
		},
		{
			"sub_topic": "summary", "entity_key": "last_strike",
			"component": "sensor", "name": "Last Strike",
			// device_class timestamp requires an ISO-8601 state; an empty render
			// keeps Home Assistant from trying to parse a placeholder.
			"value_template": "{{ value_json.last_strike_utc | default('', true) }}",
			"device_class":   "timestamp",
			"icon":           "mdi:clock-outline",
		},
		{
			"sub_topic": "summary", "entity_key": "storm_active",
			"component": "binary_sensor", "name": "Storm Activity",
			"value_template": "{{ 'ON' if value_json.storm_active else 'OFF' }}",
			"icon":           "mdi:weather-lightning",
		},
		{
			"sub_topic": "summary", "entity_key": "iq_stream",
			"component": "binary_sensor", "name": "IQ Stream",
			"value_template":  "{{ 'ON' if value_json.iq_connected else 'OFF' }}",
			"device_class":    "connectivity",
			"entity_category": "diagnostic",
		},
	}
}

// declareEntities registers every Home Assistant entity. Idempotent — UberSDR
// treats a repeat declaration as an in-place update, so running this on each
// (re)connection costs nothing.
func (p *MQTTPublisher) declareEntities() {
	entities := haEntities()
	declared := 0

	for _, e := range entities {
		e["addon_version"] = addonVersion
		e["addon_model"] = addonModel

		body, err := json.Marshal(e)
		if err != nil {
			log.Printf("[mqtt] declare: marshal %v: %v", e["entity_key"], err)
			continue
		}

		resp, err := p.client.Post(p.base+"/discovery", "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("[mqtt] declare %v: %v", e["entity_key"], err)
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()

		if resp.StatusCode >= 400 {
			log.Printf("[mqtt] declare %v rejected (%s): %s",
				e["entity_key"], resp.Status, bytes.TrimSpace(msg))
			continue
		}
		declared++
	}

	log.Printf("[mqtt] declared %d/%d Home Assistant entities", declared, len(entities))
}

// ---------------------------------------------------------------------------
// Publishing
// ---------------------------------------------------------------------------

// post sends a payload to a sub-topic. Errors are logged but never returned:
// telemetry must not be able to disturb the detector.
func (p *MQTTPublisher) post(subTopic string, payload interface{}, retain bool) {
	if p == nil || !p.available.Load() {
		return
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[mqtt] marshal %s: %v", subTopic, err)
		return
	}

	endpoint := p.base + "/publish/" + subTopic
	if retain {
		endpoint += "?retain=true"
	}

	resp, err := p.client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		// The receiver may be restarting. Drop back to unavailable so the next
		// summary tick re-probes (and re-declares) rather than logging per strike.
		p.unavailable("publish %s failed (%v) — will retry", subTopic, err)
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode < 300:
		return
	case resp.StatusCode == http.StatusServiceUnavailable:
		// Receiver is up but its broker is down. Transient; stay available.
	case resp.StatusCode == http.StatusTooManyRequests:
		log.Printf("[mqtt] rate limited publishing %s", subTopic)
	case resp.StatusCode == http.StatusForbidden:
		p.unavailable("no longer a recognised addon — pausing MQTT")
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Printf("[mqtt] publish %s: %s: %s", subTopic, resp.Status, bytes.TrimSpace(msg))
	}
}

// PublishStrike sends one detected sferic. The waveform is omitted — it is far
// too large for a message bus and is available over HTTP for TDOA consumers.
func (p *MQTTPublisher) PublishStrike(s StrikeEvent) {
	if p == nil || !p.available.Load() {
		return
	}
	p.post("strikes", map[string]interface{}{
		"id":             s.ID,
		"timestamp_ns":   s.TimestampNs,
		"time":           s.TimestampUTC.UTC().Format(time.RFC3339Nano),
		"peak_amplitude": s.PeakAmplitude,
		"noise_floor":    s.NoiseFloor,
		"snr_db":         s.SNRdB,
		"duration_ms":    s.DurationMs,
		"saturated":      s.Saturated,
	}, false)
}

// buildSummary assembles the retained detector-state payload.
func (p *MQTTPublisher) buildSummary() map[string]interface{} {
	recent := p.history.Recent(strikeHistoryDepth)

	cutoff := time.Now().Add(-summaryWindow)
	lastHour := 0
	for _, s := range recent {
		if s.TimestampUTC.After(cutoff) {
			lastHour++
		}
	}

	summary := map[string]interface{}{
		"strikes_total":     p.history.Count(),
		"strikes_last_hour": lastHour,
		// A storm is "active" while sferics are still arriving. Ten minutes is
		// long enough to ride out the gaps between cells without latching on to
		// a single isolated strike from an hour ago.
		"storm_active": false,
		"iq_connected": p.det != nil && p.det.Connected(),
	}

	// Live noise floor, not the value recorded at the last strike — otherwise
	// the reading would freeze between storms and could be hours stale.
	if p.det != nil {
		if nf := p.det.NoiseFloor(); nf > 0 {
			summary["noise_floor_db"] = amplitudeToDBFS(nf)
		}
	}

	if n := len(recent); n > 0 {
		last := recent[n-1]
		summary["last_strike_utc"] = last.TimestampUTC.UTC().Format(time.RFC3339)
		summary["last_snr_db"] = last.SNRdB
		summary["last_peak_amplitude"] = last.PeakAmplitude
		summary["storm_active"] = time.Since(last.TimestampUTC) < 10*time.Minute
	}

	return summary
}

// amplitudeToDBFS converts a normalised [0,1] envelope amplitude to dBFS.
// Guards log10(0), which would otherwise emit -Inf and break the JSON encode.
func amplitudeToDBFS(a float64) float64 {
	const floor = -200.0
	if a <= 0 {
		return floor
	}
	db := 20 * math.Log10(a)
	if db < floor {
		return floor
	}
	return db
}

// PublishSummary sends the retained detector-state payload.
func (p *MQTTPublisher) PublishSummary() {
	if p == nil {
		return
	}
	// Re-probe while dormant so the addon recovers on its own if the receiver
	// was restarted, or had MQTT enabled, after this container started.
	if !p.available.Load() && !p.probe() {
		return
	}
	p.post("summary", p.buildSummary(), true)
}

// Run publishes the summary on a ticker until ctx is cancelled, and forwards
// every strike arriving on in to MQTT before passing it along to out.
//
// Sitting in the middle of the channel rather than tapping it keeps the
// detector and the SSE hub unaware of MQTT entirely: the hub receives exactly
// what it did before.
func (p *MQTTPublisher) Run(ctx context.Context, in <-chan StrikeEvent, out chan<- StrikeEvent) {
	ticker := time.NewTicker(summaryInterval)
	defer ticker.Stop()

	// Publish an initial summary so Home Assistant has values immediately
	// rather than after the first tick.
	p.PublishSummary()

	for {
		select {
		case <-ctx.Done():
			return

		case s := <-in:
			p.PublishStrike(s)
			// Never block the detector: the SSE hub dropping events is the
			// pre-existing behaviour and must be preserved.
			select {
			case out <- s:
			default:
			}

		case <-ticker.C:
			p.PublishSummary()
		}
	}
}
