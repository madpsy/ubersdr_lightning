# ⚡ ubersdr_lightning

A VLF lightning sferic detector addon for [UberSDR](https://ubersdr.org).

Connects to UberSDR in `iq48` mode (48 kHz IQ, centred at 25 kHz, covering 1–49 kHz) and detects lightning sferics in real time using an adaptive IIR noise floor and threshold trigger. The 25 kHz centre keeps the lower band edge at 1 kHz — safely above DC — while the upper edge at 49 kHz spans the full VLF sferic spectrum. Detected strikes are displayed in a live web UI with waveform captures suitable for TDOA cross-correlation between stations.

---

## Features

- **Real-time VLF sferic detection** — IIR adaptive noise floor + threshold trigger on the IQ envelope
- **GPS-synchronised timestamps** — nanosecond-precision timestamps from UberSDR's v2 PCM packet header for TDOA cross-correlation
- **Waveform capture** — ±10 ms window around each strike peak, stored and displayed as a canvas sparkline
- **Shape validation** — duration gate (0.5–10 ms) + single-peak check rejects 50/60 Hz interference transients
- **5-second warm-up** — IIR noise floor settles before the trigger is armed, preventing false triggers on connection
- **Live web UI** — dark-themed dashboard with:
  - Full-screen flash animation on each strike
  - 60-second scrolling activity bar (log-scale SNR height, colour-coded)
  - Latest strike panel with SNR gauge and waveform canvas
  - Waveform gallery (last 12 strikes, clickable thumbnails)
  - Strike log table (newest-first, colour-coded SNR)
  - Live stats: total strikes, last strike time, best SNR, strike rate/min
- **SSE live feed** — two-message protocol: metadata (~150 bytes) + waveform (~7.5 KB) as separate named events
- **REST API** — `GET /api/strikes?n=N` returns full strike history including waveforms
- **Proxy-aware** — reads `X-Forwarded-Prefix` header from UberSDR's addon proxy; all JS API calls are correctly prefixed

---

## Requirements

- [UberSDR](https://ubersdr.org) running and accessible
- Docker + Docker Compose v2
- A VLF-capable antenna (ferrite rod, large loop, or long wire) connected to the SDR

---

## Quick Install

```bash
curl -fsSL https://raw.githubusercontent.com/madpsy/ubersdr_lightning/main/install.sh | bash
```

Then edit `~/ubersdr/lightning/docker-compose.yml` to set your `UBERSDR_URL` and run:

```bash
cd ~/ubersdr/lightning && ./restart.sh
```

---

## UberSDR Proxy Configuration

Add this addon via **UberSDR Admin → Addon Proxies**:

| Field         | Value       |
|---------------|-------------|
| Name          | `lightning` |
| Host          | `lightning` |
| Port          | `6097`      |
| Enabled       | `true`      |
| Strip prefix  | `true`      |
| Rate Limit    | `100`       |

Then access the web UI at: `http://your-ubersdr-host/addon/lightning/`

---

## Configuration

All settings are environment variables (set in `docker-compose.yml`):

| Variable              | Default                | Description |
|-----------------------|------------------------|-------------|
| `UBERSDR_URL`         | `ws://ubersdr:8080/ws` | UberSDR WebSocket URL |
| `WEB_PORT`            | `6097`                 | HTTP listen port |
| `CENTRE_HZ`           | `25000`                | IQ centre frequency in Hz (25 kHz → covers 1–49 kHz at ±24 kHz IQ BW) |
| `IIR_ALPHA`           | `0.9999`               | IIR noise floor tracking speed (higher = slower, ~2 s time constant) |
| `THRESHOLD_RATIO`     | `8.0`                  | Trigger threshold: envelope > noise × ratio (8.0 = 18 dB above noise). Raise to 10–12 in noisy RF environments. |
| `REFRACTORY_MS`       | `100`                  | Milliseconds to ignore new triggers after a confirmed strike |
| `MAX_STRIKES_PER_MIN` | `20`                   | Rate limit: suppress triggers if more than this many strikes/minute |

---

## Detection Algorithm

```
IQ samples (48 kHz, S16LE interleaved)
    │
    ▼
envelope = √(I² + Q²)  per sample pair
    │
    ▼
IIR noise floor (α = 0.9999, ~2 s time constant)
    │  (updated only when idle — sferic doesn't inflate floor)
    ▼
threshold = noiseFloor × thresholdRatio
    │
    ▼
State machine:
  IDLE ──(envelope > threshold)──► ARMED
  ARMED ──(envelope drops below threshold)──► validate:
    • duration 0.5–10 ms  ✓
    • peak in first half of window (single-peak check)  ✓
    ──► CAPTURE (collect ±10 ms waveform)
    ──► emit StrikeEvent
```

**Warm-up**: the first 5 seconds after connection are used to settle the IIR noise floor. The trigger is not armed during this period.

---

## API

| Endpoint                                  | Method | Description |
|-------------------------------------------|--------|-------------|
| `/`                                       | GET    | Web UI (index.html) |
| `/api/events`                             | GET    | SSE stream: full StrikeEvents + spectrum + waveform frames |
| `/api/events?minimal=1`                   | GET    | SSE stream: compact strike-only events (no spectrum/waveform) |
| `/api/strikes`                            | GET    | JSON array of last 100 strikes (includes waveforms, ~750 KB) |
| `/api/strikes?n=N`                        | GET    | JSON array of last N strikes (max 1000) |
| `/api/strikes?since=5m`                   | GET    | Strikes in the last 5 minutes |
| `/api/strikes?since=1h&n=200`             | GET    | Up to 200 strikes in the last hour |
| `/api/strikes?minimal=1`                  | GET    | Last 100 strikes, **no waveforms** (~150 bytes each) |
| `/api/strikes?since=5m&minimal=1`         | GET    | Last 5 min, no waveforms — lightweight polling |
| `/api/spectrum`                           | GET    | JSON: latest FFT spectrum (4096 bins, dBFS, 1–49 kHz) |
| `/api/status`                             | GET    | JSON: strike count + server time |

### `?since` duration format

Accepts Go duration syntax: `5m`, `1h`, `30s`, `2h30m`, `90s` etc.

### Full SSE stream (`/api/events`)

Four event types are broadcast:

1. **Unnamed message** (`onmessage`) — strike metadata (~150 bytes, no waveform)
2. **`event: waveform`** — `{"id":"...","waveform":[...960 floats...]}` for the waveform gallery
3. **`event: spectrum`** — FFT spectrum frame every 5 s (base64 float32 array, 4096 bins)
4. **`event: connected`** / **`event: heartbeat`** — connection lifecycle

### Minimal SSE stream (`/api/events?minimal=1`)

For external clients, scripts, and IoT devices. Only compact strike messages are sent — no spectrum, no waveform, no named events except heartbeat.

```
data: {"time":"15:30:00.123","peak_amplitude":0.4231,"snr_db":14.3,"duration_ms":3.25,"noise_floor":0.00812,"saturated":false}
```

**Example — Python client:**
```python
import sseclient, requests

resp = requests.get('http://localhost:6097/api/events?minimal=1', stream=True)
for event in sseclient.SSEClient(resp):
    if event.data and event.data != '{}':
        import json
        strike = json.loads(event.data)
        print(f"{strike['time']}  SNR {strike['snr_db']:.1f} dB  {strike['duration_ms']:.2f} ms")
```

**Example — curl:**
```bash
curl -N 'http://localhost:6097/api/events?minimal=1'
```

### StrikeEvent JSON (full, from `/api/strikes`)

```json
{
  "id":               "uuid",
  "timestamp_ns":     1718123456789012345,
  "timestamp_utc":    "2024-06-11T15:30:00.000Z",
  "peak_amplitude":   0.04231,
  "noise_floor":      0.00812,
  "snr":              5.21,
  "snr_db":           14.3,
  "duration_samples": 156,
  "duration_ms":      3.25,
  "saturated":        false,
  "waveform":         [0.001, 0.003, ...]
}
```

> **Note**: `waveform` is omitted from SSE metadata messages (sent as a separate `event: waveform` SSE event) but included in `GET /api/strikes` responses.

### Spectrum JSON (from `/api/spectrum`)

```json
{
  "bins":          [-85.2, -83.1, ...],
  "bin_count":     4096,
  "freq_start_hz": 1000.0,
  "freq_end_hz":   48988.3,
  "bin_width_hz":  11.71875
}
```

---

## TDOA Cross-Correlation

Each `StrikeEvent` carries a GPS-synchronised `timestamp_ns` (nanosecond Unix timestamp from UberSDR's v2 PCM packet header) and a `waveform` array of ~960 normalised envelope samples (±10 ms at 48 kHz).

To cross-correlate two stations:

1. Fetch recent strikes from both stations via `GET /api/strikes`
2. Match strikes by approximate timestamp (within ±50 ms)
3. Cross-correlate the `waveform` arrays to find the time delay
4. Multiply delay (samples) × sample period (20.83 µs) to get TDOA in seconds
5. Use TDOA + station coordinates to compute a hyperbolic line of position

---

## Building from Source

```bash
git clone https://github.com/madpsy/ubersdr_lightning
cd ubersdr_lightning
go build ./...
./ubersdr_lightning -url ws://your-ubersdr:8080/ws -listen :6097
```

### Docker

```bash
./docker.sh build   # linux/amd64
./docker.sh arm64   # linux/arm64 (Raspberry Pi)
./docker.sh push    # multi-platform push to Docker Hub
./docker.sh run     # run locally
```

---

## File Structure

```
ubersdr_lightning/
├── main.go           — entry point, flag/env parsing
├── lightning.go      — IQ stream consumer, sferic detector, SSE hub
├── pcm_decoder.go    — UberSDR v2 PCM binary packet decoder (zstd, IQ helpers)
├── web.go            — HTTP server (SSE, REST API, embedded static files)
├── go.mod            — Go module (github.com/madpsy/ubersdr_lightning)
├── static/
│   ├── index.html    — web UI (Go template, injects BASE_PATH)
│   └── app.js        — all client-side JS (SSE, canvas, gallery, table)
├── Dockerfile
├── docker-compose.yml
├── install.sh        — one-line installer
├── docker.sh         — build/push/run helper
├── entrypoint.sh     — Docker entrypoint (env → flags)
├── start.sh / stop.sh / restart.sh / update.sh
└── README.md
```

---

## Licence

MIT
