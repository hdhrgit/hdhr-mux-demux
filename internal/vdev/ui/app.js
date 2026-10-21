(() => {
'use strict';

/* ---------- helpers ---------- */
const $ = (sel, el = document) => el.querySelector(sel);
const $$ = (sel, el = document) => Array.from(el.querySelectorAll(sel));
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const patch = (el, html) => { if (el && el.__h !== html) { el.innerHTML = html; el.__h = html; } };
const mbps = (bps) => (bps / 1e6).toFixed(bps >= 1e7 ? 0 : 1);
const mhz = (f) => (f ? `${+(f / 1e6).toFixed(3)} MHz` : '');
const pct = (v) => Math.max(0, Math.min(100, Math.round(v)));
const clock = (sec) => new Date(sec * 1000).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${String(m).padStart(2, '0')}m`;
  if (m) return `${m}m ${String(s).padStart(2, '0')}s`;
  return `${s}s`;
}
const EXT = '<svg viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M5 2H2.5v7.5H10V7M7 2h3v3M10 2L5.5 6.5"/></svg>';
// a physical tuner's id, linking to the device's own status page for it
const phys = (id, url) => (url ? `<a class="phys mono" href="${esc(url)}" target="_blank" rel="noopener" title="Open this tuner's own status page">${esc(id)}${EXT}</a>` : `<span class="mono">${esc(id)}</span>`);
const level = (v) => (v >= 70 ? '' : v >= 40 ? 'warn' : 'bad');

/* ---------- appearance ---------- */
const THEMES = {
  light: [
    { id: 'paper', name: 'Paper', note: 'warm, easy on the eyes', bg: '#f3eee4', dot: '#bf5328' },
    { id: 'mist', name: 'Mist', note: 'cool and clean', bg: '#eaeff6', dot: '#2b67e0' },
    { id: 'mint', name: 'Mint', note: 'soft green', bg: '#e8f1ec', dot: '#178a62' },
    { id: 'lilac', name: 'Lilac', note: 'soft violet', bg: '#efebf6', dot: '#7448c9' },
  ],
  dark: [
    { id: 'slate', name: 'Slate', note: 'blue-grey', bg: '#1b2130', dot: '#6ea8fe' },
    { id: 'midnight', name: 'Midnight', note: 'deep navy', bg: '#0f1527', dot: '#8aa4ff' },
    { id: 'dusk', name: 'Dusk', note: 'muted purple', bg: '#211c2f', dot: '#c89bf2' },
    { id: 'ember', name: 'Ember', note: 'warm brown', bg: '#231d1a', dot: '#ff9d5c' },
  ],
};
const DEFAULTS = { mode: 'system', light: 'paper', dark: 'slate' };
const KEY = 'hdhrmux.theme';
const mq = window.matchMedia ? matchMedia('(prefers-color-scheme: dark)') : null;

function loadSettings() {
  try { return Object.assign({}, DEFAULTS, JSON.parse(localStorage.getItem(KEY) || '{}')); } catch (e) { return Object.assign({}, DEFAULTS); }
}
function saveSettings(s) { try { localStorage.setItem(KEY, JSON.stringify(s)); } catch (e) { /* private mode: keep it for this visit */ } }
let settings = loadSettings();
const override = new URLSearchParams(location.search).get('theme'); // ?theme=midnight, for sharing and screenshots

function effectiveTheme() {
  if (override && [...THEMES.light, ...THEMES.dark].some((t) => t.id === override)) return override;
  if (settings.mode === 'light') return settings.light;
  if (settings.mode === 'dark') return settings.dark;
  return mq && mq.matches ? settings.dark : settings.light;
}
function applyTheme() { document.documentElement.setAttribute('data-theme', effectiveTheme()); }
if (mq && mq.addEventListener) mq.addEventListener('change', () => { if (settings.mode === 'system') applyTheme(); });

function renderThemePopover() {
  const pop = $('#theme-pop');
  const seg = ['system', 'light', 'dark'].map((m) =>
    `<button type="button" data-mode="${m}" aria-pressed="${settings.mode === m}">${{ system: 'System', light: 'Light', dark: 'Dark' }[m]}</button>`).join('');
  const sw = (kind) => THEMES[kind].map((t) =>
    `<button type="button" class="swatch" data-kind="${kind}" data-id="${t.id}" aria-pressed="${settings[kind] === t.id}">
       <span class="chip" style="background:${t.bg}"><i style="background:${t.dot}"></i></span>
       <span>${t.name}<small>${t.note}</small></span></button>`).join('');
  pop.innerHTML = `
    <h3>Mode</h3><div class="seg" role="group" aria-label="Mode">${seg}</div>
    <h3>Light theme</h3><div class="swatches">${sw('light')}</div>
    <h3>Dark theme</h3><div class="swatches">${sw('dark')}</div>
    <p class="hint">${settings.mode === 'system' ? 'System follows your device: it switches between your light and dark theme on its own.' : 'Saved in this browser.'}</p>`;
}
function setupThemeMenu() {
  const btn = $('#theme-btn'), pop = $('#theme-pop');
  const close = () => { pop.hidden = true; btn.setAttribute('aria-expanded', 'false'); };
  btn.addEventListener('click', (e) => {
    e.stopPropagation();
    if (pop.hidden) { renderThemePopover(); pop.hidden = false; btn.setAttribute('aria-expanded', 'true'); } else close();
  });
  pop.addEventListener('click', (e) => {
    e.stopPropagation();
    const m = e.target.closest('[data-mode]'), s = e.target.closest('.swatch');
    if (m) settings.mode = m.dataset.mode;
    else if (s) {
      settings[s.dataset.kind] = s.dataset.id;
      if (settings.mode !== 'system') settings.mode = s.dataset.kind; // show what was just picked
    } else return;
    saveSettings(settings); applyTheme(); renderThemePopover();
  });
  document.addEventListener('click', close);
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !pop.hidden) { close(); btn.focus(); } });
}

/* ---------- data ---------- */
async function getJSON(url) {
  const r = await fetch(url, { cache: 'no-store' });
  if (!r.ok) throw new Error(`${url}: ${r.status}`);
  return r.json();
}
const conn = $('#conn');
function setConn(ok) {
  conn.className = `conn ${ok ? 'ok' : 'bad'}`;
  $('span', conn).textContent = ok ? 'live' : 'offline, retrying';
}

/* ---------- activity page ---------- */
const history = new Map(); // tuner index -> recent incoming bitrates
const HIST = 60;
let timer = null, stopped = false;

function spark(values) {
  if (values.length < 6) return '<svg class="spark" viewBox="0 0 120 34" preserveAspectRatio="none" aria-hidden="true"><line class="base" x1="0" y1="31" x2="120" y2="31"/></svg>';
  const max = Math.max(1, ...values), n = values.length;
  const pts = values.map((v, i) => `${(i / (HIST - 1) * 120 + (HIST - n) / (HIST - 1) * 120).toFixed(1)},${(32 - v / max * 28).toFixed(1)}`);
  const first = pts[0].split(',')[0], last = pts[pts.length - 1].split(',')[0];
  return `<svg class="spark" viewBox="0 0 120 34" preserveAspectRatio="none" aria-hidden="true">
    <polygon class="area" points="${first},34 ${pts.join(' ')} ${last},34"/><polyline class="line" points="${pts.join(' ')}"/></svg>`;
}
function meter(label, v) {
  v = pct(v);
  return `<div class="sig"><label>${label}<b>${v}%</b></label><div class="meter ${level(v)}" role="meter" aria-label="${label}" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${v}"><i style="width:${v}%"></i></div></div>`;
}
function programme(p, nowSec) {
  if (!p) return '<div class="v-prog"><div class="title none">No guide data for this channel yet</div></div>';
  const span = Math.max(1, p.end - p.start), done = pct((nowSec - p.start) / span * 100);
  const head = p.placeholder ? '<div class="title tba">To be announced</div><div class="tba-note">the station has not published this slot</div>' : `<div class="title">${esc(p.title)}</div>`;
  return `<div class="v-prog">${head}
    <div class="progress" role="progressbar" aria-valuenow="${done}" aria-valuemin="0" aria-valuemax="100"><i style="width:${done}%"></i></div>
    <div class="times"><span>${clock(p.start)}</span><span>${Math.max(0, Math.round((p.end - nowSec) / 60))} min left · ends ${clock(p.end)}</span></div></div>`;
}
function viewerHTML(v, nowSec) {
  const raw = !v.scan && (!/^\d/.test(v.channel) || /^ch/.test(v.channel));
  const queue = pct(v.queue);
  return `<div class="viewer">
    <div class="v-top"><span class="chan ${raw ? 'raw' : ''}">${esc(v.channel)}</span><span class="v-name">${esc(v.name || (v.scan ? '' : 'Unlisted channel'))}</span>
      <span class="v-client mono">${esc(v.client || 'unknown')}${v.control ? ' · control' : ''}</span></div>
    ${v.scan ? '' : programme(v.now, nowSec)}
    ${v.next ? `<div class="times"><span>Next</span><span>${v.next.placeholder ? 'to be announced' : esc(v.next.title)} · ${clock(v.next.start)}</span></div>` : ''}
    <div class="v-meta"><span>watching <b>${dur(nowSec - v.since)}</b></span><span><b>${(v.bps / 1e6).toFixed(1)}</b> Mbps</span>
      <span class="buf">buffer <span class="meter ${queue > 60 ? 'bad' : queue > 25 ? 'warn' : ''}"><i style="width:${queue}%"></i></span> ${queue}%</span>
      ${v.drops ? `<span class="drops">${v.drops} dropped</span>` : ''}</div></div>`;
}
function otherHTML(o, holder, nowSec) {
  const raw = !/^\d/.test(o.channel);
  return `<div class="viewer">
    <div class="v-top"><span class="chan ${raw ? 'raw' : ''}">${esc(o.channel)}</span><span class="v-name">${esc(o.name || '')}</span>
      <span class="v-client mono">${esc(holder || 'another application')}</span></div>
    ${o.now ? programme(o.now, nowSec) : ''}
    ${o.next ? `<div class="times"><span>Next</span><span>${o.next.placeholder ? 'to be announced' : esc(o.next.title)} · ${clock(o.next.start)}</span></div>` : ''}</div>`;
}
function tunerHTML(t, nowSec) {
  const sig = t.signal || { strength: 0, quality: 0, symbol: 0 };
  const state = { watching: 'Watching', other: 'Other app', idle: 'Idle' }[t.state];
  return `<div class="card tuner ${t.state}">
    <div class="tuner-head"><div class="tuner-num">${t.index}</div>
      <div class="tuner-title"><b>Tuner ${t.index}</b><span>${phys(t.physical, t.physicalUrl)}${t.frequency ? ' · ' + mhz(t.frequency) : ''}</span></div>
      <span class="chip-state ${t.state}">${state}</span></div>
    <div class="signals">${meter('Signal', sig.strength)}${meter('Quality', sig.quality)}${meter('Symbols', sig.symbol)}
      <div>${spark(history.get(t.index) || [])}<div class="rate"><b>${(t.rxBps / 1e6).toFixed(1)}</b> Mbps in</div></div></div>
    ${t.state === 'other' ? `<div class="other-note">In use by another application${t.holder ? `: <span class="mono">${esc(t.holder)}</span>${t.holderLocal ? ' (this machine)' : ''}` : ''}${t.transport ? ` · receiving over ${esc(t.transport)}` : ''}.<br>The multiplexer leaves it alone.${t.holderLocal ? ' A program on this machine looks just like the multiplexer to the device, since both use this address.' : ''}</div>` : ''}
    ${t.state === 'other' && t.watching && t.watching.length ? `<div class="viewers">${t.watching.map((o) => otherHTML(o, t.holder, nowSec)).join('')}</div>` : ''}
    ${t.viewers.length ? `<div class="viewers">${t.viewers.map((v) => viewerHTML(v, nowSec)).join('')}</div>` : ''}</div>`;
}
function statCard(label, value, unit, sub, extra = '', cls = '') {
  return `<div class="card stat ${cls}"><div class="label">${label}</div><div class="value">${value}<small>${unit}</small></div><div class="sub">${sub}</div>${extra}</div>`;
}

function updateActivity(a) {
  const nowSec = a.time / 1000, S = a.summary;
  patch($('#setup-wrap'), (a.setup ? `<div class="banner warn" role="status" style="margin-top:14px"><b>Nothing to serve yet</b><p>${esc(a.setup)} <a href="#/settings">Open Settings</a></p></div>` : '') + (a.notice ? `<div class="banner" role="status" style="margin-top:14px"><b>Not discoverable</b><p>${esc(a.notice)} <a href="#/settings">Open Settings</a></p></div>` : ''));
  $('#device-sub').textContent = `${a.device.model} · ${a.device.id}${a.device.version ? ' · v' + a.device.version.replace(/^v/, '') : ''} · up ${dur(a.device.uptime)}`;
  $('#foot-note').textContent = `Updated ${new Date(a.time).toLocaleTimeString()}`;
  for (const t of a.tuners) {
    const h = history.get(t.index) || [];
    h.push(t.rxBps); if (h.length > HIST) h.shift(); history.set(t.index, h);
  }
  const busy = S.tunersInUse + S.otherTuners; // in use by anybody
  const useMine = S.tuners ? pct(S.tunersInUse / S.tuners * 100) : 0, useOther = S.tuners ? pct(S.otherTuners / S.tuners * 100) : 0;
  patch($('#stats'),
    statCard('Watching now', S.viewers, S.viewers === 1 ? 'viewer' : 'viewers', S.viewers ? `on ${S.tunersInUse} tuner${S.tunersInUse === 1 ? '' : 's'}` : 'nobody is watching', '', 'accent') +
    statCard('Tuners in use', busy, `/ ${S.tuners}`, `${S.tuners - busy} free${busy ? ` · ${S.tunersInUse} here, ${S.otherTuners} by other apps` : ''}`, `<div class="bar"><i style="width:${useMine}%"></i><i class="other" style="width:${useOther}%"></i></div>`) +
    statCard('From the tuners', mbps(S.rxBps), 'Mbps', 'pulled over the network') +
    statCard('To viewers', mbps(S.txBps), 'Mbps', S.rxBps > 0 && S.txBps > S.rxBps * 1.1 ? `${(S.txBps / S.rxBps).toFixed(1)}× shared` : 'delivered') +
    statCard('Devices', S.physicalOnline, `/ ${S.physical}`, S.physicalOnline === S.physical ? 'all tuners online' : `${S.physical - S.physicalOnline} offline`));

  // fewest viewers first (then by tuner): cards in a row are then about as tall as each other, with no gaps under the short ones
  const active = a.tuners.filter((t) => t.state !== 'idle').sort((x, y) => (x.viewers || []).length - (y.viewers || []).length || x.index - y.index);
  const idle = a.tuners.filter((t) => t.state === 'idle');
  $('#n-active').textContent = active.length;
  patch($('#tuners'), active.length ? active.map((t) => tunerHTML(t, nowSec)).join('') :
    `<div class="card empty" style="grid-column:1/-1"><h3>Quiet right now</h3>Nobody is watching. All ${S.tuners} tuners are free.</div>`);
  patch($('#idle-wrap'), idle.length && active.length ? `<div class="section-title">Free tuners <span class="count">${idle.length}</span></div>
    <div class="idle-row">${idle.map((t) => `<span class="pill"><b>${t.index}</b>${phys(t.physical, t.physicalUrl)}</span>`).join('')}</div>` : '');

  const bg = a.streams.filter((s) => s.background || s.lingering);
  patch($('#bg-wrap'), bg.length ? `<div class="section-title">Behind the scenes <span class="count">${bg.length}</span></div>
    <div class="bg-list">${bg.map((s) => `<div class="card bg-item"><span class="dot ${s.background ? 'on' : ''}"></span><div><b>${s.background ? 'Collecting the guide' : 'Held for a quick return'}</b>
      <small>${phys(s.tuner, s.physicalUrl)} · ${mhz(s.frequency)} · ${(s.rxBps / 1e6).toFixed(1)} Mbps</small></div></div>`).join('')}</div>` : '');
  const sy = a.system;
  if (sy) {
    const mb = (n) => (n / 1048576).toFixed(0);
    patch($('#sys-wrap'), `<div class="section-title">This machine</div><div class="cards sys">` +
      statCard('CPU', sy.cpuPercent.toFixed(0), '%', `of one core, last minute · ${sy.cores} core${sy.cores === 1 ? '' : 's'}`) +
      statCard('Memory', mb(sy.rssBytes), 'MB', sy.memLimitBytes ? `of ${mb(sy.memLimitBytes)} MB allowed` : 'resident') +
      (sy.load1 || sy.load5 ? statCard('Load', sy.load1.toFixed(2), '', `5 min ${sy.load5.toFixed(2)}`) : '') +
      (sy.tempC ? statCard('Temperature', sy.tempC.toFixed(0), '°C', sy.throttled ? 'throttled or under-voltage' : 'CPU') : '') +
      statCard('Process', sy.goroutines, 'goroutines', `${sy.openFds} open files`) + '</div>');
  }
  const c = a.counters;
  patch($('#counters'), `<span><b>${c.streamsOpened}</b> tunings</span><span><b>${c.failovers}</b> failovers</span><span><b>${c.filterUpdates}</b> filter updates</span>` +
    `<span><b>${c.reclaims}</b> idle tuners reclaimed</span><span><b>${c.droppedBatches}</b> dropped batches</span>`);
}

function renderActivity(root) {
  root.innerHTML = `<div class="page-title"><h1>Activity</h1><p>Who is watching what, right now.</p></div>
    <div class="cards" id="stats">${'<div class="skeleton"></div>'.repeat(5)}</div>
    <div id="setup-wrap"></div>
    <div class="section-title">Active tuners <span class="count" id="n-active">–</span></div>
    <div class="tuners" id="tuners"></div><div id="idle-wrap"></div><div id="bg-wrap"></div><div class="counters" id="counters"></div><div id="sys-wrap"></div>`;
  const tick = async () => {
    clearTimeout(timer);
    if (stopped) return;
    try { updateActivity(await getJSON('/ui/api/activity')); setConn(true); } catch (e) { setConn(false); }
    if (!stopped) timer = setTimeout(tick, document.hidden ? 8000 : 2000);
  };
  tick();
}

/* ---------- lineup page ---------- */
const lineup = { data: [], q: '', filter: 'all', loaded: false };
const chans = { q: '', data: null }; // the channels page
function lineupRows() {
  const q = lineup.q.trim().toLowerCase(), nowSec = Date.now() / 1000;
  return lineup.data.filter((c) => {
    if (lineup.filter === 'hd' && !c.hd) return false;
    if (lineup.filter === 'live' && !c.watching) return false;
    if (lineup.filter === 'fav' && !c.favorite) return false;
    if (!q) return true;
    return `${c.number} ${c.name} ${c.now && !c.now.placeholder ? c.now.title : ''} ${c.next && !c.next.placeholder ? c.next.title : ''}`.toLowerCase().includes(q);
  }).map((c) => {
    const p = c.now, done = p ? pct((nowSec - p.start) / Math.max(1, p.end - p.start) * 100) : 0;
    return `<tr data-url="${esc(c.url)}"><td class="c-num"><span class="chan">${esc(c.number)}</span></td>
      <td><b class="nm">${esc(c.name)}</b>${c.hd ? '<span class="tag">HD</span>' : ''}${c.modulation === 'atsc3' ? '<span class="tag">3.0</span>' : ''}${c.watching ? `<span class="tag live">${c.watching} watching</span>` : ''}</td>
      <td class="prog">${p ? `<div class="title">${p.placeholder ? '<i style="color:var(--muted);font-weight:450">To be announced</i>' : esc(p.title)}</div><div class="progress"><i style="width:${done}%"></i></div><small>${clock(p.start)} – ${clock(p.end)}</small>` : '<small>no guide data</small>'}</td>
      <td class="prog c-next">${c.next ? `<div class="title">${c.next.placeholder ? '<i style="color:var(--muted);font-weight:450">To be announced</i>' : esc(c.next.title)}</div><small>${clock(c.next.start)}</small>` : ''}</td>
      <td class="c-rf">${mhz(c.frequency)}${c.codec ? `<br><small>${esc(c.codec)}</small>` : ''}</td>
      <td class="c-act"><button class="btn" data-copy>Copy URL</button></td></tr>`;
  }).join('');
}
function updateLineup() {
  const rows = lineupRows();
  $('#lineup-count').textContent = lineup.data.length;
  patch($('#lineup-body'), rows || `<tr><td colspan="6"><div class="empty"><h3>No channels match</h3>Try a different search or filter.</div></td></tr>`);
}
async function copyText(text) {
  try { await navigator.clipboard.writeText(text); return true; } catch (e) {
    const ta = document.createElement('textarea'); ta.value = text; document.body.appendChild(ta); ta.select();
    let ok = false; try { ok = document.execCommand('copy'); } catch (e2) { /* ignore */ }
    ta.remove(); return ok;
  }
}
function renderLineup(root) {
  root.innerHTML = `<div class="page-title"><h1>Lineup</h1><p><span id="lineup-count">–</span> channels, with what is on now.</p></div>
    <div class="toolbar"><label class="search"><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="M20 20l-3.5-3.5"/></svg>
      <input id="q" type="search" placeholder="Search channels and programmes" aria-label="Search channels and programmes" autocomplete="off"></label>
      <div class="filters" role="group" aria-label="Filter">${[['all', 'All'], ['hd', 'HD'], ['live', 'Being watched'], ['fav', 'Favourites']].map(([k, n]) => `<button type="button" data-f="${k}" aria-pressed="${lineup.filter === k}">${n}</button>`).join('')}</div>
      <a class="btn" href="/playlist.m3u8" title="For players that cannot use the HDHomeRun protocol; guide link included">M3U8 playlist</a></div>
    <div class="card table-card"><table class="lineup"><thead><tr><th class="c-num">Ch</th><th>Channel</th><th>Now</th><th class="c-next">Next</th><th class="c-rf">RF</th><th class="c-act"></th></tr></thead><tbody id="lineup-body"><tr><td colspan="6"><div class="skeleton" style="margin:12px"></div></td></tr></tbody></table></div>`;
  const q = $('#q'); q.value = lineup.q;
  q.addEventListener('input', () => { lineup.q = q.value; updateLineup(); });
  $$('.filters button', root).forEach((b) => b.addEventListener('click', () => {
    lineup.filter = b.dataset.f; $$('.filters button', root).forEach((x) => x.setAttribute('aria-pressed', String(x === b))); updateLineup();
  }));
  $('#lineup-body').addEventListener('click', async (e) => {
    const b = e.target.closest('[data-copy]'); if (!b) return;
    const url = b.closest('tr').dataset.url, ok = await copyText(url);
    b.textContent = ok ? 'Copied' : 'Copy failed'; b.classList.toggle('ok', ok);
    setTimeout(() => { b.textContent = 'Copy URL'; b.classList.remove('ok'); }, 1400);
  });
  const load = async () => {
    clearTimeout(timer);
    if (stopped) return;
    try { lineup.data = (await getJSON('/ui/api/lineup')).channels; setConn(true); lineup.loaded = true; updateLineup(); } catch (e) { setConn(false); }
    if (!stopped) timer = setTimeout(load, 20000);
  };
  load();
}

/* ---------- settings page ---------- */
const GROUPS = ['General', 'Network', 'Devices', 'Streaming', 'Lineup', 'Guide', 'Advanced', 'Diagnostics'];
let settingsState = null;

function ctl(f, v, locked, env) {
  const id = 'f-' + f.key.replace(/\W/g, '_'), dis = locked || env ? 'disabled' : '';
  const ph = f.placeholder ? `placeholder="${esc(f.placeholder)}"` : '';
  if (f.type === 'bool') return `<label class="switch"><input type="checkbox" id="${id}" data-key="${esc(f.key)}" ${v ? 'checked' : ''} ${dis}><span class="slider"></span></label>`;
  if (f.type === 'choice') return `<select id="${id}" data-key="${esc(f.key)}" ${dis}>${f.choices.map((c) => `<option ${c === v ? 'selected' : ''}>${esc(c)}</option>`).join('')}</select>`;
  if (f.type === 'list') return `<textarea id="${id}" data-key="${esc(f.key)}" rows="${Math.max(2, Math.min(6, (v || []).length + 1))}" ${ph} ${dis} spellcheck="false">${esc((v || []).join('\n'))}</textarea>`;
  const shown = f.type === 'int' && (v === 0 || v === undefined) ? '' : (v ?? '');
  return `<input id="${id}" data-key="${esc(f.key)}" type="text" value="${esc(shown)}" ${ph} ${dis} spellcheck="false" autocomplete="off">`;
}
function fieldHTML(f, st) {
  const v = st.values[f.key], saved = st.saved.includes(f.key), env = st.env[f.key];
  return `<div class="field ${f.type === 'bool' ? 'inline' : ''}" data-field="${esc(f.key)}">
    <div class="field-text"><label for="f-${esc(f.key.replace(/\W/g, '_'))}">${esc(f.label)}</label>${f.restart ? ' <span class="tag" title="Read once, at start: a change applies after a restart.">restart</span>' : ''}
      <p>${esc(f.help)}</p>
      ${(st.pendingRestart || []).includes(f.key) ? '<p class="note pending">Saved, but still running with the old value until the multiplexer restarts.</p>' : ''}
      ${env ? `<p class="note">Set by ${esc(env)}, which overrides this page.</p>` : ''}
      ${saved && !st.locked ? '<p class="note saved">Saved from this page. <button type="button" class="link" data-reset>Use the default again</button></p>' : ''}</div>
    <div class="field-ctl">${ctl(f, v, st.locked, env)}</div></div>`;
}
function readControl(el, f) {
  if (f.type === 'bool') return el.checked;
  if (f.type === 'list') return el.value.split('\\n').map((x) => x.trim()).filter(Boolean);
  return el.value.trim();
}
function sameValue(a, b) { return JSON.stringify(a) === JSON.stringify(b); }

function renderSettings(root) {
  root.innerHTML = '<div class="page-title"><h1>Settings</h1><p>Change how the multiplexer runs without editing files.</p></div><div id="settings-body"><div class="skeleton"></div></div>';
  const load = async () => {
    try { settingsState = await getJSON('/ui/api/settings'); setConn(true); drawSettings(); } catch (e) {
      setConn(false);
      $('#settings-body').innerHTML = '<div class="card empty"><h3>Settings are not available</h3>This setup does not offer them, or this address is not on a private network.</div>';
    }
  };
  load();
}

function drawSettings(message) {
  const st = settingsState, body = $('#settings-body');
  const fields = Object.fromEntries(st.schema.map((f) => [f.key, f]));
  const sto = st.storage;
  const banner = sto.warning ? `<div class="banner warn" role="alert"><b>${sto.kind === 'container-layer' || sto.kind === 'memory' ? 'Nothing here is kept' : 'Storage is not permanent'}</b><p>${esc(sto.warning)}</p></div>` : '';
  const lock = st.locked ? '<div class="banner"><b>Settings are locked</b><p>settings_locked is set in the config file, so this page only shows the values.</p></div>' : '';
  const waiting = (st.pendingRestart || []).map((k) => (fields[k] || {}).label || k);
  const pendingBanner = waiting.length ? `<div class="banner warn" role="status"><b>Waiting for a restart</b><p>${esc(waiting.join(', '))} ${waiting.length === 1 ? 'was' : 'were'} saved and will take effect when the multiplexer restarts.${st.restartable ? '' : ' Restart the container or service to apply.'}</p></div>` : '';
  const setupBanner = st.setup ? `<div class="banner warn" role="status"><b>Nothing to serve yet</b><p>${esc(st.setup)}</p></div>` : '';
  const noticeBanner = st.notice ? `<div class="banner" role="status"><b>Not discoverable</b><p>${esc(st.notice)}</p></div>` : '';
  body.innerHTML = setupBanner + noticeBanner + banner + lock + pendingBanner + GROUPS.map((g) => {
    const fs = st.schema.filter((f) => f.group === g);
    return fs.length ? `<section class="card settings-group"><h2>${g}</h2>${fs.map((f) => fieldHTML(f, st)).join('')}</section>` : '';
  }).join('') + `<div class="card info-card"><h2>Where things are</h2><dl>
      <dt>State directory</dt><dd><code>${esc(sto.path)}</code> <span class="tag">${esc({ disk: 'on disk', mounted: 'mapped', 'anonymous-volume': 'anonymous volume', 'container-layer': 'inside the container', memory: 'memory' }[sto.kind] || sto.kind)}</span>${sto.writable ? '' : ' <span class="tag bad">not writable</span>'}</dd>
      <dt>Config file</dt><dd><code>${esc(st.configPath || '(none: defaults and environment)')}</code></dd>
      <dt>Saved from this page</dt><dd><code>${esc(st.settingsPath)}</code></dd></dl></div>
    <div class="savebar" id="savebar"><span id="save-msg"></span><span class="grow"></span>${st.restartable ? '<button type="button" class="btn" id="restart" title="Restart the multiplexer. Viewers are disconnected for a few seconds.">Restart</button>' : ''}<button type="button" class="btn" id="discard" disabled>Discard</button><button type="button" class="btn primary" id="save" disabled>Apply</button></div>`;
  const pending = {};
  const bar = $('#savebar');
  if (message) { $('#save-msg').textContent = message; bar.classList.add('done'); }
  const update = () => {
    const n = Object.keys(pending).length;
    $('#save').disabled = $('#discard').disabled = n === 0 || st.locked;
    bar.classList.remove('error');
    if (n) { bar.classList.remove('done'); $('#save-msg').textContent = n + ' change' + (n === 1 ? '' : 's') + ' not applied yet'; }
    else if (!bar.classList.contains('done')) $('#save-msg').textContent = '';
  };
  body.addEventListener('input', (e) => {
    const el = e.target.closest('[data-key]'); if (!el) return;
    const f = fields[el.dataset.key], v = readControl(el, f), orig = st.values[f.key];
    const same = f.type === 'int' ? String(v === '' ? 0 : v) === String(orig ?? 0) : sameValue(v, f.type === 'list' ? (orig || []) : orig);
    if (same) delete pending[f.key]; else pending[f.key] = v === '' && f.type !== 'text' ? null : v;
    update();
  });
  body.addEventListener('click', async (e) => {
    const reset = e.target.closest('[data-reset]');
    if (reset) { pending[reset.closest('.field').dataset.field] = null; reset.closest('.note').textContent = 'Will use the default after saving.'; update(); return; }
    if (e.target.id === 'discard') { drawSettings(); return; }
    if (e.target.id === 'restart') { restartNow(); return; }
    if (e.target.id !== 'save') return;
    $('#save').disabled = true;
    try {
      const r = await fetch('/ui/api/settings', { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'hdhr-mux-ui' }, body: JSON.stringify({ changes: pending }) });
      const j = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(j.error || 'applying failed (' + r.status + ')');
      Object.assign(st, { values: j.values, saved: j.saved, pendingRestart: j.pendingRestart || [] });
      const later = (j.pendingRestart || []).filter((k) => k in pending).length;
      drawSettings('Applied.' + (later ? ' ' + later + ' of them take effect after a restart.' : ' No restart needed.'));
    } catch (err) { $('#save-msg').textContent = err.message; bar.classList.add('error'); $('#save').disabled = false; $('#discard').disabled = false; }
  });
}

async function restartNow() {
  if (!confirm('The multiplexer will restart itself. Viewers are disconnected for a few seconds. Continue?')) return;
  $('#save-msg').textContent = 'Restarting…';
  $('#restart').disabled = true;
  try { await fetch('/ui/api/restart', { method: 'POST', headers: { 'X-Requested-With': 'hdhr-mux-ui' } }); } catch (e) { /* it may already be gone */ }
  const until = Date.now() + 90000;
  await new Promise((r) => setTimeout(r, 2500));
  while (Date.now() < until) {
    try { const r = await fetch('/ui/api/activity', { cache: 'no-store' }); if (r.ok) { location.reload(); return; } } catch (e) { /* not back yet */ }
    await new Promise((r) => setTimeout(r, 1500));
  }
  $('#save-msg').textContent = 'It has not come back after 90 seconds. Check its log (docker logs, journalctl -u hdhr-mux).';
}

/* ---------- channels page: where each channel is on the air, and the choices ---------- */
function guideCell(l) {
  if (!l.guide) return '<span class="muted" title="This RF channel has not been read yet">not read yet</span>';
  if (!l.guideEvents) return '<span class="bad-text" title="This RF channel sends no EPG for the channel">none</span>';
  return `<b>${l.guideHours >= 10 ? Math.round(l.guideHours) : l.guideHours.toFixed(1)}</b> h`;
}
function signalCell(l) {
  const s = l.signal;
  if (!s) return '<span class="muted" title="Not measured yet">–</span>';
  const age = Math.max(0, Math.round(Date.now() / 1000 - s.at));
  return `<span title="Measured through device ${esc(s.device)} ${dur(age)} ago: signal strength, signal quality, symbol quality">` +
    `<b class="${level(s.ss)}">${s.ss}</b> / <b class="${level(s.snq)}">${s.snq}</b> / <b class="${level(s.seq)}">${s.seq}</b></span>`;
}
function chanControls(c, l, ed) {
  const dis = ed ? '' : 'disabled';
  const id = (k) => `${k}-${c.number}-${l.frequency}`;
  const allow = `<td class="c-ctl"><input type="checkbox" id="${id('a')}" data-act="allow" data-freq="${l.frequency}" ${l.off ? '' : 'checked'} ${dis} aria-label="Use ${mhz(l.frequency)} for video"></td>`;
  return allow +
    `<td class="c-ctl"><input type="radio" name="first-${esc(c.number)}" data-act="first" data-freq="${l.frequency}" ${l.first ? 'checked' : ''} ${l.off ? 'disabled' : dis} aria-label="Try ${mhz(l.frequency)} first"></td>` +
    `<td class="c-ctl"><input type="radio" name="guide-${esc(c.number)}" data-act="guide" data-freq="${l.frequency}" ${l.used ? 'checked' : ''} ${dis} aria-label="Take the EPG from ${mhz(l.frequency)}"></td>`;
}
function locationRows(c, ed) {
  return c.locations.map((l) => `<tr class="${l.chosen ? 'chosen' : ''} ${l.off ? 'off' : ''}">
    <td class="mono">${mhz(l.frequency)}</td>
    <td>${esc(l.name || c.name)}${l.chosen ? ' <span class="tag" title="Where the devices\' lineups put this channel">lineup</span>' : ''}</td>
    <td class="mono">${l.program}${l.modulation === 'atsc3' ? ' <span class="tag">3.0</span>' : ''}</td>
    <td>${signalCell(l)}</td><td>${guideCell(l)}</td>${chanControls(c, l, ed)}</tr>`).join('');
}
function channelNotes(c, ed) {
  const used = c.locations.find((l) => l.used);
  const first = c.locations.find((l) => l.first);
  const bits = [];
  bits.push(`Video: tried first on <b>${first ? mhz(first.frequency) : 'nothing'}</b>${c.video ? ' (your choice) ' + (ed ? '<button type="button" class="link" data-act="reset-first">use the lineup\'s place</button>' : '') : ''}; the others are used when the first cannot be tuned or is already open.`);
  bits.push(`EPG: ${c.guide ? `from <b>${mhz(c.guide)}</b> (your choice) ` + (ed ? '<button type="button" class="link" data-act="reset-guide">use automatic</button>' : '') : `automatic${used ? ` (<b>${mhz(used.frequency)}</b>, the one that goes furthest ahead)` : ': there is no EPG for this channel yet'}`}.`);
  return `<p class="chan-notes">${bits.join('<br>')}</p>`;
}
function channelCard(c, ed) {
  const multi = c.locations.length > 1;
  return `<div class="card chan-card ${multi ? 'multi' : ''}" data-num="${esc(c.number)}"><div class="chan-head"><span class="chan">${esc(c.number)}</span><b>${esc(c.name)}</b>
    ${multi ? `<span class="tag" title="This channel is received on more than one RF channel">${c.locations.length} RF channels</span>` : ''}${c.hd ? ' <span class="tag">HD</span>' : ''}${c.drm ? ' <span class="tag" title="Encrypted">DRM</span>' : ''}</div>
    ${c.locations.length ? `<table class="loc-table"><thead><tr><th>RF channel</th><th>Name there</th><th>Program</th><th>Signal <small>strength / quality / symbols</small></th><th>EPG ahead</th><th class="c-ctl" title="May be used for video">Video</th><th class="c-ctl" title="Tried first for video">First</th><th class="c-ctl" title="The programme guide (EPG) is taken from here">EPG</th></tr></thead><tbody>${locationRows(c, ed)}</tbody></table>${multi ? channelNotes(c, ed) : ''}`
      : '<p class="muted pad">Not located yet: the multiplexer finds out the first time it tunes this channel.</p>'}</div>`;
}
function updateChannels() {
  const d = chans.data; if (!d) return;
  const ed = d.editable;
  const q = chans.q.trim().toLowerCase();
  let list = d.channels.filter((c) => c.locations.length > 1);
  if (q) list = list.filter((c) => (c.number + ' ' + c.name + ' ' + c.locations.map((l) => l.name + ' ' + mhz(l.frequency)).join(' ')).toLowerCase().includes(q));
  $('#chan-count').textContent = `${d.duplicates} channel${d.duplicates === 1 ? '' : 's'} on multiple RF channels, ${d.known} RF channel${d.known === 1 ? '' : 's'} read so far`;
  const hint = !d.known ? '<div class="banner" role="status"><b>Still learning</b><p>This table fills in as the multiplexer tunes channels and collects the EPG (a pass runs about an hour apart; the first one starts a minute or two after start-up).</p></div>' : '';
  const ro = !ed ? '<div class="banner" role="status"><b>Read only</b><p>Choices can only be changed from the local network, and not while settings are locked.</p></div>' : '';
  patch($('#chan-list'), ro + hint + (list.length ? list.map((c) => channelCard(c, ed)).join('') :
    `<div class="card empty"><h3>${q ? 'Nothing matches' : 'No channel is on more than one RF channel (yet)'}</h3>${q ? 'Try a different search.' : 'Channels that are carried on two RF channels show up here once the multiplexer has read both. Whether a channel is on or off is decided in the Lineup.'}</div>`));
}
async function saveChannel(number, body) {
  const r = await fetch('/ui/api/channels/' + encodeURIComponent(number), { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'hdhr-mux-ui' }, body: JSON.stringify(body) });
  if (!r.ok) { const j = await r.json().catch(() => ({})); throw new Error(j.error || 'saving failed (' + r.status + ')'); }
}
function renderChannels(root) {
  root.innerHTML = `<div class="page-title"><h1>Advanced</h1><p id="chan-count">–</p></div>
    <div class="toolbar"><label class="search"><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="M20 20l-3.5-3.5"/></svg>
      <input id="cq" type="search" placeholder="Search channels, names and RF channels" aria-label="Search" autocomplete="off"></label></div>
    <div id="chan-msg" class="chan-msg" role="status" hidden></div><div id="chan-list"><div class="skeleton"></div></div>`;
  const q = $('#cq'); q.value = chans.q;
  q.addEventListener('input', () => { chans.q = q.value; updateChannels(); });
  const load = async () => {
    clearTimeout(timer);
    if (stopped) return;
    try { chans.data = await getJSON('/ui/api/channels'); setConn(true); updateChannels(); } catch (e) { setConn(false); }
    if (!stopped) timer = setTimeout(load, 8000);
  };
  const act = async (e) => {
    const el = e.target.closest('[data-act]'); if (!el) return;
    if (el.tagName === 'BUTTON' && e.type !== 'click') return;
    if (el.tagName !== 'BUTTON' && e.type !== 'change') return;
    const row = el.closest('[data-num]'); if (!row) return;
    const c = chans.data.channels.find((x) => x.number === row.dataset.num); if (!c) return;
    const freq = Number(el.dataset.freq || 0);
    let off = c.locations.filter((l) => l.off).map((l) => l.frequency), video = c.video || 0, guide = c.guide || 0;
    switch (el.dataset.act) {
      case 'allow': if (el.checked) off = off.filter((f) => f !== freq); else { if (!off.includes(freq)) off.push(freq); if (video === freq) video = 0; } break;
      case 'first': video = freq; off = off.filter((f) => f !== freq); break;
      case 'guide': guide = freq; break;
      case 'reset-first': video = 0; break;
      case 'reset-guide': guide = 0; break;
      default: return;
    }
    const msg = $('#chan-msg');
    try { await saveChannel(c.number, { off, video, guide }); msg.hidden = true; } catch (err) { msg.textContent = c.number + ': ' + err.message; msg.hidden = false; }
    clearTimeout(timer); load();
  };
  $('#chan-list').addEventListener('change', act);
  $('#chan-list').addEventListener('click', act);
  load();
}

/* ---------- router ---------- */
const PAGES = { activity: renderActivity, lineup: renderLineup, advanced: renderChannels, settings: renderSettings };
function route() {
  stopped = true; clearTimeout(timer);
  let name = (location.hash.replace(/^#\/?/, '') || 'activity').split('/')[0];
  if (name === 'channels') name = 'advanced'; // its old name
  const page = PAGES[name] ? name : 'activity';
  $$('.tabs a').forEach((a) => { if (a.dataset.tab === page) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current'); });
  document.title = `${{ activity: 'Activity', lineup: 'Lineup', advanced: 'Advanced', settings: 'Settings' }[page]} · ${$('.brand-text b').textContent}`;
  stopped = false;
  PAGES[page]($('#app'));
}
document.addEventListener('visibilitychange', () => { if (!document.hidden && !stopped) route(); });
window.addEventListener('hashchange', route);
applyTheme();
setupThemeMenu();
route();
})();

