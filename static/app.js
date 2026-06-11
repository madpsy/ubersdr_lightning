'use strict';
const BASE        = window.BASE_PATH || '';
const MAX_TABLE   = 200;
const MAX_GALLERY = 12;
const ACTIVITY_S  = 60;

// State
let totalStrikes = 0;
let tableRows    = 0;
let bestSNRdB    = -Infinity;
const actBuf     = [];  // {ts:Date, snrDB:number}
const gallery    = [];  // StrikeEvent[] newest-first

// DOM refs (populated after DOMContentLoaded)
let connPill, connLabel, hdrTotal, hdrRate;
let statTotal, statLast, statLastSub, statSNR, statRate;
let latestInner, galleryInner, galleryCount;
let tbody, tableCount;
let actCanvas, actCtx;
let flashEl, tooltip;

// ── Init ───────────────────────────────────────────────────────────────────
document.addEventListener('DOMContentLoaded', () => {
  connPill    = document.getElementById('conn-pill');
  connLabel   = document.getElementById('conn-label');
  hdrTotal    = document.getElementById('hdr-total');
  hdrRate     = document.getElementById('hdr-rate');
  statTotal   = document.getElementById('stat-total');
  statLast    = document.getElementById('stat-last');
  statLastSub = document.getElementById('stat-last-sub');
  statSNR     = document.getElementById('stat-snr');
  statRate    = document.getElementById('stat-rate');
  latestInner = document.getElementById('latest-inner');
  galleryInner= document.getElementById('gallery-inner');
  galleryCount= document.getElementById('gallery-count');
  tbody       = document.getElementById('strikes-tbody');
  tableCount  = document.getElementById('table-count');
  actCanvas   = document.getElementById('activity-canvas');
  actCtx      = actCanvas.getContext('2d');
  flashEl     = document.getElementById('flash-overlay');
  tooltip     = document.getElementById('tooltip');

  actCanvas.addEventListener('mousemove', onActivityHover);
  actCanvas.addEventListener('mouseleave', () => { tooltip.className = ''; });

  loadHistory();
  connect();
  setInterval(drawActivity, 1000);
  setInterval(() => {
    const rate = calcRate();
    if (statRate) statRate.textContent = rate.toFixed(1);
    if (hdrRate)  hdrRate.textContent  = rate.toFixed(1);
  }, 5000);
});

// ── SSE ────────────────────────────────────────────────────────────────────
// pendingWaveforms holds waveform data for live strikes that arrived before
// the waveform SSE event was processed (race-free merge by strike ID).
const pendingWaveforms = new Map(); // id → waveform []

function connect() {
  const es = new EventSource(BASE + '/api/events');

  es.addEventListener('connected', () => {
    connPill.className = 'conn-pill connected';
    connLabel.textContent = 'live';
  });

  es.addEventListener('heartbeat', () => {});

  es.onerror = () => {
    connPill.className = 'conn-pill disconnected';
    connLabel.textContent = 'reconnecting…';
  };

  // Unnamed message = strike metadata (no waveform)
  es.onmessage = e => {
    try { onStrike(JSON.parse(e.data), true); } catch(_) {}
  };

  // Named "waveform" event = {id, waveform} for the most recent strike
  es.addEventListener('waveform', e => {
    try {
      const { id, waveform } = JSON.parse(e.data);
      if (!id || !waveform) return;

      // Find the matching strike in the gallery array and attach the waveform
      const s = gallery.find(g => g.id === id);
      if (s) {
        // Strike already in gallery (onmessage arrived first) — attach waveform
        // and redraw its thumbnail
        s.waveform = waveform;
        rebuildGallery();
        // Also update the latest panel if this is the most recent strike
        if (gallery[0] && gallery[0].id === id) {
          updateLatest(gallery[0]);
        }
      } else {
        // Waveform arrived before the strike metadata — stash it
        pendingWaveforms.set(id, waveform);
        // Clean up stale entries (keep last 20)
        if (pendingWaveforms.size > 20) {
          const oldest = pendingWaveforms.keys().next().value;
          pendingWaveforms.delete(oldest);
        }
      }
    } catch(_) {}
  });
}

// ── Load history ───────────────────────────────────────────────────────────
async function loadHistory() {
  try {
    const resp = await fetch(BASE + '/api/strikes?n=100');
    if (!resp.ok) return;
    const data = await resp.json();
    if (!Array.isArray(data) || data.length === 0) return;
    // data is oldest-first; process oldest first so newest ends up at top
    data.forEach(s => onStrike(s, false));
  } catch(_) {}
}

// ── Strike handler ─────────────────────────────────────────────────────────
function onStrike(s, live) {
  totalStrikes++;
  const db = s.snr_db || 0;
  if (db > bestSNRdB) bestSNRdB = db;
  actBuf.push({ ts: new Date(s.timestamp_ns / 1e6), snrDB: db });

  // Check if a waveform arrived ahead of the metadata (rare race condition)
  if (!s.waveform && s.id && pendingWaveforms.has(s.id)) {
    s.waveform = pendingWaveforms.get(s.id);
    pendingWaveforms.delete(s.id);
  }

  updateStats(s);
  updateLatest(s);
  addGalleryEntry(s);
  prependTableRow(s);
  drawActivity();
  if (live) triggerFlash();
}

// ── Flash ──────────────────────────────────────────────────────────────────
function triggerFlash() {
  flashEl.classList.remove('flash');
  void flashEl.offsetWidth;
  flashEl.classList.add('flash');
}

// ── Stats ──────────────────────────────────────────────────────────────────
function updateStats(s) {
  hdrTotal.textContent  = totalStrikes;
  statTotal.textContent = totalStrikes;
  const utc = new Date(s.timestamp_ns / 1e6);
  statLast.textContent    = utc.toISOString().slice(11, 23);
  statLastSub.textContent = utc.toISOString().slice(0, 10) + ' UTC';
  if (bestSNRdB > -Infinity) statSNR.textContent = bestSNRdB.toFixed(1);
  const rate = calcRate();
  statRate.textContent = rate.toFixed(1);
  hdrRate.textContent  = rate.toFixed(1);
}

function calcRate() {
  const cutoff = Date.now() - ACTIVITY_S * 1000;
  return actBuf.filter(x => x.ts.getTime() >= cutoff).length / (ACTIVITY_S / 60);
}

// ── Latest strike ──────────────────────────────────────────────────────────
function snrClass(db) {
  return db >= 20 ? 'snr-hi' : db >= 12 ? 'snr-med' : 'snr-lo';
}

function updateLatest(s) {
  const db     = s.snr_db || 0;
  const sc     = snrClass(db);
  const utc    = new Date(s.timestamp_ns / 1e6).toISOString().replace('T',' ').replace('Z',' UTC');
  const snrPct = Math.min(100, Math.max(0, (db / 40) * 100));

  let html = `
    <div class="latest-ts"><strong>${utc}</strong></div>
    <div class="kv-grid">
      <div class="kv hi"><div class="k">Peak Amp</div><div class="v">${s.peak_amplitude.toFixed(5)}</div></div>
      <div class="kv ${sc}"><div class="k">SNR</div><div class="v">${db.toFixed(1)} dB</div></div>
      <div class="kv"><div class="k">Duration</div><div class="v">${(s.duration_ms||0).toFixed(2)} ms</div></div>
      <div class="kv"><div class="k">Noise Floor</div><div class="v">${s.noise_floor.toFixed(5)}</div></div>
    </div>
    <div class="snr-bar-wrap">
      <div class="snr-bar-label"><span>SNR</span><span>${db.toFixed(1)} dB</span></div>
      <div class="snr-bar-track"><div class="snr-bar-fill" style="width:${snrPct}%"></div></div>
    </div>`;

  if (s.waveform && s.waveform.length > 0) {
    html += `<div class="wf-label">Waveform (±10 ms)</div>
             <canvas id="waveform-canvas" width="280" height="72"></canvas>`;
  }
  latestInner.innerHTML = html;
  if (s.waveform && s.waveform.length > 0) {
    drawWaveformCanvas(document.getElementById('waveform-canvas'), s.waveform, true);
  }
}

// ── Waveform drawing ───────────────────────────────────────────────────────
function drawWaveformCanvas(canvas, waveform, showMarker) {
  const ctx = canvas.getContext('2d');
  const W = canvas.width, H = canvas.height;
  ctx.clearRect(0, 0, W, H);
  if (!waveform || waveform.length < 2) return;
  const max = Math.max(...waveform, 1e-9);
  const n   = waveform.length;

  // Gradient fill under curve
  const grad = ctx.createLinearGradient(0, 0, 0, H);
  grad.addColorStop(0, 'rgba(245,200,66,0.35)');
  grad.addColorStop(1, 'rgba(245,200,66,0.02)');
  ctx.beginPath();
  waveform.forEach((v, i) => {
    const x = (i / (n-1)) * W;
    const y = H - (v / max) * (H - 4) - 2;
    i === 0 ? ctx.moveTo(x, y) : ctx.lineTo(x, y);
  });
  ctx.lineTo(W, H); ctx.lineTo(0, H); ctx.closePath();
  ctx.fillStyle = grad; ctx.fill();

  // Waveform line
  ctx.beginPath();
  waveform.forEach((v, i) => {
    const x = (i / (n-1)) * W;
    const y = H - (v / max) * (H - 4) - 2;
    i === 0 ? ctx.moveTo(x, y) : ctx.lineTo(x, y);
  });
  ctx.strokeStyle = '#f5c842'; ctx.lineWidth = 1.5; ctx.stroke();

  // Trigger marker (pre/post boundary)
  if (showMarker) {
    const mid = Math.floor(n / 2);
    const mx  = (mid / (n-1)) * W;
    ctx.strokeStyle = 'rgba(255,77,109,0.6)';
    ctx.lineWidth = 1; ctx.setLineDash([3,3]);
    ctx.beginPath(); ctx.moveTo(mx, 0); ctx.lineTo(mx, H); ctx.stroke();
    ctx.setLineDash([]);
  }
}

// ── Gallery ────────────────────────────────────────────────────────────────
function addGalleryEntry(s) {
  if (!s.waveform || s.waveform.length === 0) return;
  gallery.unshift(s);
  if (gallery.length > MAX_GALLERY) gallery.pop();
  rebuildGallery();
}

function rebuildGallery() {
  if (gallery.length === 0) {
    galleryInner.innerHTML = '<div class="gallery-empty">No waveforms yet</div>';
    galleryCount.textContent = '0';
    return;
  }
  galleryCount.textContent = gallery.length;
  galleryInner.innerHTML = '';
  gallery.forEach(s => {
    const div = document.createElement('div');
    div.className = 'wf-thumb';
    const ts = new Date(s.timestamp_ns / 1e6).toISOString().slice(11, 23);
    const db = (s.snr_db || 0).toFixed(1);
    div.innerHTML = `<canvas width="120" height="44"></canvas>
      <div class="wf-meta">
        <span>${ts}</span>
        <span class="snr-val">${db} dB</span>
      </div>`;
    div.addEventListener('click', () => updateLatest(s));
    galleryInner.appendChild(div);
    drawWaveformCanvas(div.querySelector('canvas'), s.waveform, false);
  });
}

// ── Table ──────────────────────────────────────────────────────────────────
function prependTableRow(s) {
  const ph = tbody.querySelector('.no-data');
  if (ph) ph.parentElement.remove();

  tableRows++;
  const utc = new Date(s.timestamp_ns / 1e6).toISOString().replace('T',' ').slice(0, 23);
  const db  = s.snr_db || 0;
  const sc  = snrClass(db);
  const tr  = document.createElement('tr');
  tr.className = 'new-row';
  tr.innerHTML = `
    <td class="seq-col">${tableRows}</td>
    <td class="ts-col">${utc}</td>
    <td>${s.timestamp_ns}</td>
    <td>${s.peak_amplitude.toFixed(4)}</td>
    <td class="${sc}">${db.toFixed(1)}</td>
    <td>${(s.duration_ms||0).toFixed(2)} ms</td>`;
  tbody.insertBefore(tr, tbody.firstChild);
  while (tbody.rows.length > MAX_TABLE) tbody.deleteRow(tbody.rows.length - 1);
  tableCount.textContent = Math.min(tableRows, MAX_TABLE) + ' entries';
}

// ── Activity bar ───────────────────────────────────────────────────────────
function onActivityHover(e) {
  const rect  = actCanvas.getBoundingClientRect();
  const mx    = e.clientX - rect.left;
  const now   = Date.now();
  const wMs   = ACTIVITY_S * 1000;
  const W     = actCanvas.width;

  // Find nearest strike within 8px
  let best = null, bestDx = 9;
  actBuf.forEach(item => {
    const age = now - item.ts.getTime();
    if (age > wMs) return;
    const x = W - (age / wMs) * W;
    const dx = Math.abs(x - mx);
    if (dx < bestDx) { bestDx = dx; best = item; }
  });

  if (best) {
    const ts = best.ts.toISOString().slice(11, 23);
    tooltip.textContent = `${ts}  SNR ${best.snrDB.toFixed(1)} dB`;
    tooltip.style.left = (e.clientX + 12) + 'px';
    tooltip.style.top  = (e.clientY - 28) + 'px';
    tooltip.className  = 'show';
  } else {
    tooltip.className = '';
  }
}

function drawActivity() {
  if (!actCanvas) return;
  const W = actCanvas.offsetWidth;
  const H = actCanvas.offsetHeight;
  if (W === 0 || H === 0) return;
  actCanvas.width  = W;
  actCanvas.height = H;

  const now  = Date.now();
  const wMs  = ACTIVITY_S * 1000;

  // Prune old entries
  while (actBuf.length > 0 && now - actBuf[0].ts.getTime() > wMs) actBuf.shift();

  actCtx.clearRect(0, 0, W, H);

  // Grid lines every 10 s
  actCtx.strokeStyle = 'rgba(30,45,66,0.9)';
  actCtx.lineWidth = 1;
  for (let s = 0; s <= ACTIVITY_S; s += 10) {
    const x = Math.round((s / ACTIVITY_S) * W) + 0.5;
    actCtx.beginPath(); actCtx.moveTo(x, 0); actCtx.lineTo(x, H - 14); actCtx.stroke();
  }

  // Strike bars — height uses log10(snrDB) scale, clamped
  actBuf.forEach(item => {
    const age   = now - item.ts.getTime();
    const x     = W - (age / wMs) * W;
    const alpha = 0.3 + 0.7 * (1 - age / wMs);
    // log scale: 0 dB → 0%, 40 dB → 100%
    const norm  = Math.min(1, Math.max(0.05, item.snrDB > 0 ? Math.log10(1 + item.snrDB) / Math.log10(41) : 0.05));
    const barH  = Math.max(4, norm * (H - 18));

    // Colour by SNR
    let colour;
    if (item.snrDB >= 20)      colour = `rgba(255,77,109,${alpha.toFixed(2)})`;
    else if (item.snrDB >= 12) colour = `rgba(255,140,66,${alpha.toFixed(2)})`;
    else                       colour = `rgba(245,200,66,${alpha.toFixed(2)})`;

    actCtx.fillStyle = colour;
    actCtx.fillRect(Math.round(x) - 2, H - 14 - barH, 4, barH);

    // Glow on recent strikes (< 3 s old)
    if (age < 3000) {
      actCtx.fillStyle = colour.replace(/[\d.]+\)$/, '0.15)');
      actCtx.fillRect(Math.round(x) - 5, H - 14 - barH - 2, 10, barH + 4);
    }
  });

  // Time axis
  actCtx.fillStyle = 'rgba(107,127,153,0.7)';
  actCtx.font = '10px monospace';
  actCtx.textAlign = 'center';
  for (let s = 0; s <= ACTIVITY_S; s += 10) {
    const x = (s / ACTIVITY_S) * W;
    actCtx.fillText(`-${ACTIVITY_S - s}s`, x, H - 2);
  }
}
