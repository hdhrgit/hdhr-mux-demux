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
    <div class="v-top"><span class="chan ${raw ? 'raw' : ''}">${esc(v.channel)}</span><span class="v-name">${esc(v.name || (v.scan ? '' : 'Unlisted channel'))}</span>${v.scan ? '<span class="tag" title="This client picks PIDs that complete no programme: it is collecting tables (the guide), not watching a channel.">scan</span>' : ''}${v.inferred ? `<span class="tag" title="${v.alt && v.alt.length > 1 ? 'These channels carry the same audio and video, so there is no telling which one is being watched. ' : ''}This client picks PIDs instead of a channel: the channel is a guess from them.">guess</span>` : ''}
      <span class="v-client mono">${esc(v.client || 'unknown')}${v.control ? ' · control' : ''}</span></div>
    ${v.scan ? '' : programme(v.now, nowSec)}
    ${v.next ? `<div class="times"><span>Next</span><span>${v.next.placeholder ? 'to be announced' : esc(v.next.title)} · ${clock(v.next.start)}</span></div>` : ''}
    <div class="v-meta"><span>watching <b>${dur(nowSec - v.since)}</b></span><span><b>${(v.bps / 1e6).toFixed(1)}</b> Mbps</span>
      <span class="buf">buffer <span class="meter ${queue > 60 ? 'bad' : queue > 25 ? 'warn' : ''}"><i style="width:${queue}%"></i></span> ${queue}%</span>
      ${v.drops ? `<span class="drops">${v.drops} dropped</span>` : ''}</div></div>`;
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
    ${t.state === 'other' ? '<div class="other-note">Another application is using this tuner. The multiplexer leaves it alone.</div>' : ''}
    ${t.viewers.length ? `<div class="viewers">${t.viewers.map((v) => viewerHTML(v, nowSec)).join('')}</div>` : ''}</div>`;
}
function statCard(label, value, unit, sub, extra = '', cls = '') {
  return `<div class="card stat ${cls}"><div class="label">${label}</div><div class="value">${value}<small>${unit}</small></div><div class="sub">${sub}</div>${extra}</div>`;
}

function updateActivity(a) {
  const nowSec = a.time / 1000, S = a.summary;
  $('#device-sub').textContent = `${a.device.model} · ${a.device.id} · up ${dur(a.device.uptime)}`;
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

  const active = a.tuners.filter((t) => t.state !== 'idle');
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
  const c = a.counters;
  patch($('#counters'), `<span><b>${c.streamsOpened}</b> tunings</span><span><b>${c.failovers}</b> failovers</span><span><b>${c.filterUpdates}</b> filter updates</span>` +
    `<span><b>${c.reclaims}</b> idle tuners reclaimed</span><span><b>${c.droppedBatches}</b> dropped batches</span>`);
}

function renderActivity(root) {
  root.innerHTML = `<div class="page-title"><h1>Activity</h1><p>Who is watching what, right now.</p></div>
    <div class="cards" id="stats">${'<div class="skeleton"></div>'.repeat(5)}</div>
    <div class="section-title">Active tuners <span class="count" id="n-active">–</span></div>
    <div class="tuners" id="tuners"></div><div id="idle-wrap"></div><div id="bg-wrap"></div><div class="counters" id="counters"></div>`;
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
      <td><b>${esc(c.name)}</b>${c.hd ? '<span class="tag">HD</span>' : ''}${c.modulation === 'atsc3' ? '<span class="tag">3.0</span>' : ''}${c.watching ? `<span class="tag live">${c.watching} watching</span>` : ''}</td>
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

/* ---------- router ---------- */
const PAGES = { activity: renderActivity, lineup: renderLineup };
function route() {
  stopped = true; clearTimeout(timer);
  const name = (location.hash.replace(/^#\/?/, '') || 'activity').split('/')[0];
  const page = PAGES[name] ? name : 'activity';
  $$('.tabs a').forEach((a) => { if (a.dataset.tab === page) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current'); });
  document.title = `${page === 'activity' ? 'Activity' : 'Lineup'} · ${$('.brand-text b').textContent}`;
  stopped = false;
  PAGES[page]($('#app'));
}
document.addEventListener('visibilitychange', () => { if (!document.hidden && !stopped) route(); });
window.addEventListener('hashchange', route);
applyTheme();
setupThemeMenu();
route();
})();
