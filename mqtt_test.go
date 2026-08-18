package main

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestIngestBaseURL covers endpoint derivation — the reason this addon needs no
// new docker-compose settings.
func TestIngestBaseURL(t *testing.T) {
	os.Unsetenv("UBERSDR_INGEST_URL")

	cases := []struct {
		in, want string
	}{
		{"ws://ubersdr:8080/ws", "http://ubersdr:6926"},
		{"ws://ubersdr:8080/ws?x=1", "http://ubersdr:6926"},
		{"wss://sdr.example.com/ws", "http://sdr.example.com:6926"},
		{"ws://192.168.1.10:8080/ws", "http://192.168.1.10:6926"},
		{"", "http://ubersdr:6926"},            // fall back to the compose default
		{"://nonsense", "http://ubersdr:6926"}, // unparseable → same fallback
	}
	for _, tc := range cases {
		if got := ingestBaseURL(tc.in); got != tc.want {
			t.Errorf("ingestBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// IPv6 hosts must come back bracketed or the URL is malformed.
	if got := ingestBaseURL("ws://[fd00::1]:8080/ws"); got != "http://[fd00::1]:6926" {
		t.Errorf("IPv6 host = %q", got)
	}

	// An explicit override wins and has its trailing slash trimmed.
	t.Setenv("UBERSDR_INGEST_URL", "http://elsewhere:7000/")
	if got := ingestBaseURL("ws://ubersdr:8080/ws"); got != "http://elsewhere:7000" {
		t.Errorf("override = %q", got)
	}
}

// TestDeclarationsMatchServerRules checks every entity this addon declares
// against UberSDR's documented validation rules. Getting one of these wrong
// means the entity silently never appears in Home Assistant, so it is worth
// asserting here rather than discovering it on a live receiver.
func TestDeclarationsMatchServerRules(t *testing.T) {
	var (
		entityKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
		subTopicRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(/[a-z0-9][a-z0-9_-]*)*$`)
		iconRe      = regexp.MustCompile(`^mdi:[a-z0-9-]{1,40}$`)

		components  = map[string]bool{"sensor": true, "binary_sensor": true}
		stateClass  = map[string]bool{"measurement": true, "total": true, "total_increasing": true}
		entityCat   = map[string]bool{"diagnostic": true}
		deviceClass = map[string]bool{"timestamp": true, "connectivity": true, "problem": true, "signal_strength": true}
	)

	seenKeys := map[string]bool{}

	for _, e := range haEntities() {
		str := func(k string) string {
			v, _ := e[k].(string)
			return v
		}
		name := str("entity_key")
		if name == "" {
			name = str("sub_topic")
		}

		if !subTopicRe.MatchString(str("sub_topic")) {
			t.Errorf("%s: invalid sub_topic %q", name, str("sub_topic"))
		}
		if k := str("entity_key"); k != "" && !entityKeyRe.MatchString(k) {
			t.Errorf("%s: invalid entity_key %q", name, k)
		}
		if !components[str("component")] {
			t.Errorf("%s: component %q is not allowed", name, str("component"))
		}
		if str("name") == "" || len(str("name")) > 64 {
			t.Errorf("%s: name %q must be 1..64 chars", name, str("name"))
		}
		if v := str("value_template"); len(v) > 256 {
			t.Errorf("%s: value_template is %d chars (max 256)", name, len(v))
		}
		if u := str("unit_of_measurement"); len(u) > 16 {
			t.Errorf("%s: unit %q is %d chars (max 16)", name, u, len(u))
		}
		if i := str("icon"); i != "" && !iconRe.MatchString(i) {
			t.Errorf("%s: icon %q is not an mdi icon", name, i)
		}
		if c := str("entity_category"); c != "" && !entityCat[c] {
			t.Errorf("%s: entity_category %q is not allowed", name, c)
		}
		if d := str("device_class"); d != "" && !deviceClass[d] {
			t.Errorf("%s: device_class %q not in the set this test knows about", name, d)
		}
		if s := str("state_class"); s != "" {
			if !stateClass[s] {
				t.Errorf("%s: state_class %q is not allowed", name, s)
			}
			if str("component") != "sensor" {
				t.Errorf("%s: state_class is only valid on a sensor", name)
			}
		}
		// payload_on/off are sensor-invalid; we rely on the server's defaults.
		if str("component") == "sensor" && (str("payload_on") != "" || str("payload_off") != "") {
			t.Errorf("%s: payload_on/off are only valid on a binary_sensor", name)
		}

		// object_id is derived from entity_key (or sub_topic); duplicates would
		// have the second declaration rejected by the server.
		if seenKeys[name] {
			t.Errorf("duplicate entity identity %q", name)
		}
		seenKeys[name] = true
	}

	if len(seenKeys) == 0 {
		t.Fatal("no entities declared")
	}
}

// fakeIngest is a stand-in for UberSDR's ingest port.
type fakeIngest struct {
	mu           sync.Mutex
	declarations []map[string]interface{}
	published    map[string][]json.RawMessage
	retained     map[string]bool
	health       ingestHealth
	healthStatus int
}

func newFakeIngest() *fakeIngest {
	return &fakeIngest{
		published:    map[string][]json.RawMessage{},
		retained:     map[string]bool{},
		healthStatus: http.StatusOK,
		health: ingestHealth{
			Addon: "lightning", MQTTConnected: true, HADiscovery: true,
			RateLimit: 120, OfflineAfterSec: 300,
		},
	}
}

func (f *fakeIngest) server() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		status, h := f.healthStatus, f.health
		f.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		json.NewEncoder(w).Encode(h) //nolint:errcheck
	})

	mux.HandleFunc("/discovery", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var d map[string]interface{}
		if err := json.Unmarshal(body, &d); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.declarations = append(f.declarations, d)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"status": "declared"}) //nolint:errcheck
	})

	mux.HandleFunc("/publish/", func(w http.ResponseWriter, r *http.Request) {
		sub := strings.TrimPrefix(r.URL.Path, "/publish/")
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.published[sub] = append(f.published[sub], json.RawMessage(body))
		if r.URL.Query().Get("retain") == "true" {
			f.retained[sub] = true
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"status": "published"}) //nolint:errcheck
	})

	return httptest.NewServer(mux)
}

func (f *fakeIngest) countPublished(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.published[sub])
}

func (f *fakeIngest) lastPublished(sub string) map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs := f.published[sub]
	if len(msgs) == 0 {
		return nil
	}
	var out map[string]interface{}
	json.Unmarshal(msgs[len(msgs)-1], &out) //nolint:errcheck
	return out
}

// TestPublisherDeclaresAndPublishes drives the whole client path against the
// fake ingest server.
func TestPublisherDeclaresAndPublishes(t *testing.T) {
	fake := newFakeIngest()
	srv := fake.server()
	defer srv.Close()
	t.Setenv("UBERSDR_INGEST_URL", srv.URL)

	history := &StrikeHistory{}
	det := NewLightningDetector(DetectorConfig{UberSDRURL: "ws://ubersdr:8080/ws"}, history, nil, nil)
	det.connected.Store(true)
	det.noiseFloorBits.Store(math.Float64bits(0.001)) // as if the IIR had settled

	p := NewMQTTPublisher("ws://ubersdr:8080/ws", history, det)

	if !p.available.Load() {
		t.Fatal("publisher should be available against a healthy ingest server")
	}

	fake.mu.Lock()
	gotDecls := len(fake.declarations)
	fake.mu.Unlock()
	if want := len(haEntities()); gotDecls != want {
		t.Errorf("declared %d entities, want %d", gotDecls, want)
	}

	// Every declaration must carry the addon's own version and model.
	fake.mu.Lock()
	for _, d := range fake.declarations {
		if d["addon_version"] != addonVersion || d["addon_model"] != addonModel {
			t.Errorf("declaration %v missing addon version/model", d["entity_key"])
			break
		}
	}
	fake.mu.Unlock()

	// A strike publishes to "strikes", unretained, without the waveform.
	strike := StrikeEvent{
		ID: "abc", TimestampNs: 1700000000000000000,
		TimestampUTC: time.Now().UTC(), PeakAmplitude: 0.5,
		NoiseFloor: 0.001, SNRdB: 54.0, DurationMs: 1.2,
		Waveform: []float64{1, 2, 3},
	}
	p.PublishStrike(strike)

	if n := fake.countPublished("strikes"); n != 1 {
		t.Fatalf("published %d strike messages, want 1", n)
	}
	msg := fake.lastPublished("strikes")
	if _, ok := msg["waveform"]; ok {
		t.Error("waveform must not be published to MQTT — it is far too large")
	}
	if msg["snr_db"] != 54.0 {
		t.Errorf("snr_db = %v", msg["snr_db"])
	}
	fake.mu.Lock()
	strikeRetained := fake.retained["strikes"]
	fake.mu.Unlock()
	if strikeRetained {
		t.Error("strike events must not be retained — a replayed strike is wrong")
	}

	// The summary is retained so Home Assistant has values on subscribe.
	history.Add(strike)
	p.PublishSummary()

	if n := fake.countPublished("summary"); n < 1 {
		t.Fatal("summary was not published")
	}
	fake.mu.Lock()
	summaryRetained := fake.retained["summary"]
	fake.mu.Unlock()
	if !summaryRetained {
		t.Error("summary must be retained")
	}

	s := fake.lastPublished("summary")
	for _, k := range []string{"strikes_total", "strikes_last_hour", "storm_active", "iq_connected", "last_strike_utc", "last_snr_db", "noise_floor_db"} {
		if _, ok := s[k]; !ok {
			t.Errorf("summary missing %q", k)
		}
	}
	if s["strikes_total"] != float64(1) {
		t.Errorf("strikes_total = %v, want 1", s["strikes_total"])
	}
	if s["storm_active"] != true {
		t.Errorf("storm_active = %v, want true for a strike just now", s["storm_active"])
	}
}

// TestPublisherDormantWhenNotRecognised covers the receiver refusing us — the
// addon must carry on silently rather than failing.
func TestPublisherDormantWhenNotRecognised(t *testing.T) {
	fake := newFakeIngest()
	fake.healthStatus = http.StatusForbidden
	srv := fake.server()
	defer srv.Close()
	t.Setenv("UBERSDR_INGEST_URL", srv.URL)

	p := NewMQTTPublisher("ws://ubersdr:8080/ws", &StrikeHistory{}, nil)
	if p.available.Load() {
		t.Fatal("publisher must stay dormant when the receiver returns 403")
	}

	// Publishing while dormant must be a silent no-op, not a panic or a request.
	p.PublishStrike(StrikeEvent{ID: "x"})
	if n := fake.countPublished("strikes"); n != 0 {
		t.Errorf("dormant publisher sent %d messages", n)
	}
}

// TestPublisherRecoversWhenReceiverComesUp covers the receiver being restarted
// or having MQTT enabled after this addon started.
func TestPublisherRecoversWhenReceiverComesUp(t *testing.T) {
	fake := newFakeIngest()
	fake.healthStatus = http.StatusServiceUnavailable
	srv := fake.server()
	defer srv.Close()
	t.Setenv("UBERSDR_INGEST_URL", srv.URL)

	p := NewMQTTPublisher("ws://ubersdr:8080/ws", &StrikeHistory{}, nil)
	if p.available.Load() {
		t.Fatal("publisher should start dormant")
	}

	// Receiver comes up; the next summary tick must re-probe and declare.
	fake.mu.Lock()
	fake.healthStatus = http.StatusOK
	fake.mu.Unlock()

	p.PublishSummary()

	if !p.available.Load() {
		t.Fatal("publisher should have recovered on the next summary")
	}
	fake.mu.Lock()
	n := len(fake.declarations)
	fake.mu.Unlock()
	if n != len(haEntities()) {
		t.Errorf("declared %d entities after recovery, want %d", n, len(haEntities()))
	}
	if fake.countPublished("summary") != 1 {
		t.Error("summary should have been published after recovery")
	}
}

// TestNilPublisherIsSafe — a nil publisher must be a usable no-op so callers
// never branch on availability.
func TestNilPublisherIsSafe(t *testing.T) {
	var p *MQTTPublisher
	p.PublishStrike(StrikeEvent{})
	p.PublishSummary()
}

// TestAmplitudeToDBFS guards the log10(0) case, which would otherwise emit -Inf
// and make the summary payload unencodable as JSON.
func TestAmplitudeToDBFS(t *testing.T) {
	if got := amplitudeToDBFS(0); got != -200 {
		t.Errorf("amplitudeToDBFS(0) = %v, want the -200 floor", got)
	}
	if got := amplitudeToDBFS(-1); got != -200 {
		t.Errorf("amplitudeToDBFS(-1) = %v, want the -200 floor", got)
	}
	if got := amplitudeToDBFS(1); got != 0 {
		t.Errorf("amplitudeToDBFS(1) = %v, want 0 dBFS", got)
	}
	if got := amplitudeToDBFS(0.1); got < -20.1 || got > -19.9 {
		t.Errorf("amplitudeToDBFS(0.1) = %v, want ≈ -20", got)
	}

	// The summary must always encode, whatever the detector reports.
	if _, err := json.Marshal(map[string]float64{"n": amplitudeToDBFS(0)}); err != nil {
		t.Errorf("summary would fail to encode: %v", err)
	}
}

// TestRunForwardsStrikesToHub verifies the tee: MQTT sits between the detector
// and the SSE hub, and the hub must still receive every strike.
func TestRunForwardsStrikesToHub(t *testing.T) {
	fake := newFakeIngest()
	srv := fake.server()
	defer srv.Close()
	t.Setenv("UBERSDR_INGEST_URL", srv.URL)

	p := NewMQTTPublisher("ws://ubersdr:8080/ws", &StrikeHistory{}, nil)

	in := make(chan StrikeEvent, 4)
	out := make(chan StrikeEvent, 4)
	ctx, cancel := contextWithCancel()
	defer cancel()
	go p.Run(ctx, in, out)

	in <- StrikeEvent{ID: "one", TimestampUTC: time.Now().UTC()}

	select {
	case got := <-out:
		if got.ID != "one" {
			t.Errorf("hub received %q, want \"one\"", got.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("strike never reached the SSE hub — the tee is dropping events")
	}

	deadline := time.Now().Add(2 * time.Second)
	for fake.countPublished("strikes") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fake.countPublished("strikes") == 0 {
		t.Error("strike was forwarded to the hub but never published to MQTT")
	}
}

// contextWithCancel is a tiny helper so the tests read cleanly.
func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}
