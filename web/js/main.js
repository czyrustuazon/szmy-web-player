import { api, events, ApiError } from './api.js';
import { Player } from './player.js';
import { Queue } from './queue.js';
import { Visualizer } from './viz.js';
import { VirtualList } from './ui.js';
import { initUploadView } from './uploadview.js';
import { SearchState, SEP, stemOf, dirOf, highlight } from './searchstate.js';
import { $, fmtTime, icon, escapeHTML, toast, setMarquee, debounce } from './util.js';

const ROW_H = 60;
const MAX_SKIPS = 5; // consecutive undecodable tracks before auto-advance gives up

const player = new Player();
const queue = new Queue();
const search = new SearchState(); // the fuzzy search over the current track list
let searchToken = 0; // lets a newer keystroke cancel an older search

const state = {
  caps: {},
  settings: null,
  view: 'player', // player | library | favorites | upload: what the main area shows
  source: 'library', // library | favorites: where the play queue comes from
  libLoaded: false,
  libStale: false,
  uploadView: null,
  dir: '',
  parent: null,
  entries: [],
  favTracks: [],
  libTracks: null, // cached playable list for the whole library
  meta: null,
  playToken: 0,
  skips: 0,
  lastAuto: false,
  started: false,
  viz: null,
  seeking: false,
};

// ------------------------------------------------------------------ list

const list = new VirtualList($('#list'), ROW_H, renderRow);

function stem(name) {
  const i = name.lastIndexOf('.');
  return i > 0 ? name.slice(0, i) : name;
}

function fmtSize(n) {
  return n >= 1048576 ? `${(n / 1048576).toFixed(1)} MB` : `${Math.max(1, Math.round(n / 1024))} KB`;
}

function renderRow(e) {
  const row = document.createElement('div');
  const current = queue.current()?.path === e.path && !e.isDir;
  row.className = `row${current ? ' current' : ''}${!e.isDir && !e.playable ? ' dim' : ''}`;
  let sub = '';
  if (!e.isDir) {
    sub = search.active || state.source === 'favorites' ? dirOf(e.path) : `${(e.kind || 'file').toUpperCase()} · ${fmtSize(e.size)}`;
  }
  const name = e.isDir ? e.name : e.playable ? stem(e.name) : e.name;
  let nameHTML = escapeHTML(name);
  let subHTML = escapeHTML(sub);
  const marks = search.active && !e.isDir ? search.marksFor(e.path) : null;
  if (marks) {
    // Show which letters matched, in the file name and in the folder line.
    nameHTML = highlight(name, marks, 0);
    subHTML = highlight(sub, marks, stemOf(e.name).length + SEP.length);
  }
  let actions = '';
  if (e.isDir) {
    actions = icon('chev');
  } else if (e.playable) {
    actions = `<button class="ib fav${e.fav ? ' on' : ''}" data-act="fav" aria-label="${e.fav ? 'Remove from favorites' : 'Add to favorites'}">${icon(e.fav ? 'heart-fill' : 'heart')}</button>`;
    if (state.caps.canDelete) actions += `<button class="ib del" data-act="del" aria-label="Delete">${icon('trash')}</button>`;
  }
  row.innerHTML = `${icon(e.isDir ? 'folder' : 'music')}<div class="rtxt"><div class="rname">${nameHTML}</div><div class="rsub">${subHTML}</div></div><div class="ractions">${actions}</div>`;
  return row;
}

$('#list').addEventListener('click', (ev) => {
  const row = ev.target.closest('.row');
  if (!row) return;
  const e = list.items[Number(row.dataset.idx)];
  if (!e) return;
  const act = ev.target.closest('button')?.dataset.act;
  if (act === 'fav') return void toggleFav(e);
  if (act === 'del') return void deleteTrack(e);
  if (e.isDir) return void loadDir(e.path);
  if (!e.playable) return void toast('Not an audio file');
  playEntry(e);
});

async function loadDir(dir, { keepScroll = false } = {}) {
  try {
    const r = await api.browse(dir);
    state.dir = r.dir;
    state.parent = r.hasParent ? r.parent : null;
    state.entries = r.entries;
    state.libLoaded = true;
    state.libStale = false;
    renderHeader();
    list.setItems(currentItems(), { keepScroll });
    updateEmpty();
  } catch (err) {
    toast(err.message);
  }
}

async function loadFavorites() {
  try {
    state.favTracks = (await api.favorites()).tracks;
    renderHeader();
    list.setItems(currentItems());
    updateEmpty();
  } catch (err) {
    toast(err.message);
  }
}

// What the list shows: search results while searching, else the folder or the favorites.
function currentItems() {
  if (search.active) return search.items();
  return state.view === 'favorites' ? state.favTracks : state.entries;
}

// The "nothing here" hint over the list, worded for the situation.
function updateEmpty() {
  const v = state.view;
  let text = null;
  if (v === 'library' || v === 'favorites') {
    if (search.active) {
      if (!search.hits.length) text = `No matches for “${search.query}”.`;
    } else if (v === 'favorites' && !state.favTracks.length) {
      text = 'No favorites yet. Tap the heart on a track.';
    } else if (v === 'library' && state.libLoaded && !state.dir && !state.entries.length) {
      text = 'Your library is empty. Use the Upload tab to add music.';
    }
  }
  $('#empty').hidden = text === null;
  if (text !== null) $('#empty').textContent = text;
}

// ---- search

// Runs the search box's query over the tracks of the current list (the whole library, or the
// favorites) and shows the best matches. An empty box shows the normal list again.
async function applySearch() {
  const token = ++searchToken;
  const q = $('#search-input').value;
  $('#search-clear').hidden = !q;
  if (!q.trim()) {
    search.clear();
  } else {
    try {
      const pool = state.view === 'favorites' ? state.favTracks : await ensureLibTracks();
      if (token !== searchToken) return; // a newer keystroke took over
      search.run(pool, q);
    } catch (err) {
      return void toast(err.message);
    }
  }
  list.setItems(currentItems());
  renderHeader();
  updateEmpty();
}

function clearSearch() {
  searchToken++;
  $('#search-input').value = '';
  $('#search-clear').hidden = true;
  search.clear();
}

function renderHeader() {
  const v = state.view;
  let title = { player: 'Now Playing', upload: 'Upload music' }[v] || '';
  let crumbs = '';
  if (search.active && (v === 'favorites' || v === 'library')) {
    title = v === 'favorites' ? 'Search favorites' : 'Search';
    crumbs = `${search.hits.length} match${search.hits.length === 1 ? '' : 'es'}`;
  } else if (v === 'favorites') {
    title = 'Favorites';
    crumbs = `${state.favTracks.length} favorite${state.favTracks.length === 1 ? '' : 's'}`;
  } else if (v === 'library') {
    title = state.dir ? state.dir.split('/').pop() : 'Library';
    crumbs = state.dir ? `/${state.dir}` : '';
  }
  $('#title').textContent = title;
  $('#crumbs').textContent = crumbs;
  $('#btn-back').hidden = v !== 'library' || state.parent === null || search.active;
  $('#btn-locate').hidden = v !== 'library' && v !== 'favorites';
  $('#empty').hidden = true;
  document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('active', t.dataset.view === v));
}

// The mini player is only shown when something is loaded and the full player is not on screen.
function syncMini() {
  const has = !!state.meta;
  $('#mini').hidden = !has || state.view === 'player';
  document.body.classList.toggle('has-player', has && state.view !== 'player');
}

// Switches the main area: the player (home), the library, favorites or upload.
async function showView(view) {
  state.view = view;
  const isList = view === 'library' || view === 'favorites';
  if (isList) state.source = view;
  $('#list').hidden = !isList;
  $('#searchbar').hidden = !isList;
  $('#view-player').hidden = view !== 'player';
  $('#view-upload').hidden = view !== 'upload';
  syncMini();
  renderHeader();
  updateViz();
  if (view === 'player') {
    renderTime();
    if (state.meta) setMarquee($('#fp-title'), state.meta.title); // needs layout, so measure once visible
  } else if (view === 'upload') {
    state.uploadView?.show();
  } else if (view === 'favorites') {
    await loadFavorites();
  } else if (state.libStale || !state.libLoaded) {
    await loadDir(state.dir);
  } else {
    list.setItems(currentItems(), { keepScroll: true });
    updateEmpty();
  }
  if (isList && search.active) await applySearch(); // the pool differs between library and favorites
}

// Called after an upload: the library on disk changed.
function libraryChanged() {
  state.libTracks = null;
  state.libStale = true;
}

async function reloadView() {
  state.libTracks = null;
  if (state.view === 'favorites') await loadFavorites();
  else if (state.view === 'library') await loadDir(state.dir, { keepScroll: true });
  else state.libStale = true;
}

async function ensureLibTracks() {
  if (!state.libTracks) state.libTracks = (await api.tracks('')).tracks;
  return state.libTracks;
}

// ------------------------------------------------------------------ playback control

async function playEntry(e) {
  try {
    // Playing from search results queues the results, in ranked order.
    const tracks = search.active ? search.items() : state.source === 'favorites' ? state.favTracks : await ensureLibTracks();
    let idx = tracks.findIndex((t) => t.path === e.path);
    if (idx < 0 && state.source === 'library' && !search.active) {
      state.libTracks = null;
      const fresh = await ensureLibTracks();
      idx = fresh.findIndex((t) => t.path === e.path);
      queue.setQueue(fresh, idx);
    } else {
      queue.setQueue(tracks, idx);
    }
    if (queue.index < 0) return void toast('Track not found');
    state.skips = 0;
    await startTrack(queue.current(), { auto: false });
  } catch (err) {
    toast(err.message);
  }
}

// Loads metadata then hands the track to the player.
async function startTrack(t, { autoplay = true, position = 0, auto = false, silent = false } = {}) {
  const token = ++state.playToken;
  state.lastAuto = auto;
  let meta;
  try {
    meta = await api.meta(t.path);
  } catch (err) {
    if (token !== state.playToken) return;
    if (!silent) onTrackFailed(t, err.message);
    return;
  }
  if (token !== state.playToken) return;
  state.meta = meta;
  showNowPlaying(meta, t);
  ensureViz();
  const ok = await player.load(t, meta, { autoplay, position });
  if (!ok || token !== state.playToken) return;
  state.skips = 0;
  const nx = queue.peekNext();
  if (nx) api.prefetch(nx.path).catch(() => {});
}

function onTrackFailed(t, message) {
  toast(`${stem(t.name || t.path)}: ${message}`);
  // Like szmy: bad files are skipped silently during auto-advance, but never loop forever.
  if (state.lastAuto && state.skips < MAX_SKIPS) {
    state.skips++;
    advance(true);
  }
}

function advance(auto) {
  const n = queue.next(auto);
  if (!n) {
    player.pause();
    list.refresh();
    return;
  }
  list.refresh();
  startTrack(n, { auto });
}

function previous() {
  if (player.position > 3) return player.seek(0);
  const p = queue.previous();
  if (p) {
    list.refresh();
    startTrack(p, { auto: false });
  }
}

let guardAt = 0;
// Optional pocket-press guard for lock-screen next/previous (szmy's double-tap on L/R).
function guarded(fn) {
  return () => {
    if (!state.settings?.skipGuard) return fn();
    const now = Date.now();
    if (now - guardAt < 500) {
      guardAt = 0;
      return fn();
    }
    guardAt = now;
    toast('Press again to skip', { ms: 800 });
  };
}

player.addEventListener('ended', () => advance(true));
player.addEventListener('error', (ev) => {
  const t = queue.current();
  if (t) onTrackFailed(t, ev.detail.message);
  else toast(ev.detail.message);
});
player.addEventListener('state', () => {
  renderTransport();
  updateWake();
  updateViz();
  if (!player.playing) saveResume();
});
player.addEventListener('time', renderTime);

// ------------------------------------------------------------------ favorites & delete

function setFavLocal(path, on) {
  for (const arr of [state.entries, state.favTracks, state.libTracks || []]) {
    for (const e of arr) if (e.path === path) e.fav = on;
  }
  if (state.meta?.path === path) state.meta.fav = on;
}

async function toggleFav(e) {
  const on = !e.fav;
  setFavLocal(e.path, on);
  renderFavButton();
  if (state.source === 'favorites' && !on) {
    state.favTracks = state.favTracks.filter((t) => t.path !== e.path);
    search.remove(e.path);
    renderHeader();
    list.setItems(currentItems(), { keepScroll: true });
    updateEmpty();
  } else {
    list.refresh();
  }
  try {
    await api.setFavorite(e.path, on);
  } catch (err) {
    setFavLocal(e.path, !on);
    toast(err.message);
    list.refresh();
    renderFavButton();
  }
}

async function deleteTrack(e) {
  const wasPlaying = player.playing;
  let res;
  try {
    res = await api.deleteTrack(e.path);
  } catch (err) {
    return void toast(err.message);
  }
  const drop = (arr) => arr.filter((t) => t.path !== e.path);
  state.entries = drop(state.entries);
  state.favTracks = drop(state.favTracks);
  if (state.libTracks) state.libTracks = drop(state.libTracks);
  search.remove(e.path);
  list.setItems(currentItems(), { keepScroll: true });
  updateEmpty();
  renderHeader();

  const r = queue.remove(e.path);
  if (r.wasCurrent) {
    if (r.next) {
      list.refresh();
      startTrack(r.next, { auto: true, autoplay: wasPlaying }); // hand off to the next track, as szmy does
    } else {
      player.clear();
      state.meta = null;
      showNowPlaying(null);
    }
  }
  toast(`Deleted ${stem(res.name)}`, { action: 'Undo', ms: 5000, onAction: () => undoDelete(res) });
}

async function undoDelete(res) {
  try {
    await api.undo(res.token);
    await reloadView();
    toast(`Restored ${stem(res.name)}`);
  } catch (err) {
    toast(`Could not undo: ${err.message}`);
  }
}

// ------------------------------------------------------------------ now playing UI

// Renders the player screen for the loaded track, or its empty state when there is none.
function showNowPlaying(meta) {
  const has = !!meta;
  $('#view-player').classList.toggle('is-empty', !has);
  $('#np-empty').hidden = has;
  for (const id of ['#fp-play', '#fp-prev', '#fp-next', '#fp-fav', '#fp-del', '#fp-seek']) $(id).disabled = !has;
  if (!has) {
    $('#fp-art').src = '/generic.svg';
    const title = $('#fp-title');
    title.classList.remove('marquee');
    title.textContent = 'Nothing playing';
    $('#fp-artist').textContent = '';
    $('#fp-meta').textContent = '';
    $('#fp-loop').hidden = true;
    document.title = 'Master Music Player';
    syncMini();
    renderTime();
    return;
  }
  syncMini();
  const art = meta.hasArt ? api.artURL(meta.path) : '/generic.svg';
  const sub = [meta.artist, meta.album].filter(Boolean).join(' · ');
  $('#mp-art').src = art;
  $('#fp-art').src = art;
  setMarquee($('#mp-title'), meta.title);
  $('#mp-sub').textContent = sub || (meta.kind || '').toUpperCase();
  setMarquee($('#fp-title'), meta.title);
  $('#fp-artist').textContent = sub || (meta.kind || '').toUpperCase();
  $('#fp-meta').textContent = [meta.genre, meta.year, meta.track && `#${meta.track}`].filter(Boolean).join(' · ');
  renderFavButton();
  renderLoopBadge();
  document.title = `${meta.title} – Master Music Player`;
  updateMediaSession(meta);
  list.refresh();
  renderTime();
}

function renderLoopBadge() {
  const b = $('#fp-loop');
  const m = state.meta;
  if (!m?.loop) return void (b.hidden = true);
  const s = state.settings;
  const text =
    s.loopMode === 'forever' ? '∞ Loops' : s.loopMode === 'count' ? `Loops ×${s.loopCount}, then fades` : 'Loop ignored';
  b.textContent = `↻ ${text}`;
  b.hidden = false;
}

function renderFavButton() {
  const on = !!state.meta?.fav;
  const b = $('#fp-fav');
  b.classList.toggle('on', on);
  b.innerHTML = icon(on ? 'heart-fill' : 'heart');
  b.setAttribute('aria-label', on ? 'Remove from favorites' : 'Add to favorites');
}

function renderTransport() {
  const icn = player.loading ? 'spinner' : player.playing ? 'pause' : 'play';
  for (const id of ['#mp-play', '#fp-play']) {
    $(id).innerHTML = icon(icn);
    $(id).classList.toggle('spin', player.loading);
  }
  const s = state.settings;
  if (s) {
    $('#fp-shuffle').classList.toggle('on', s.shuffle);
    const rep = $('#fp-repeat');
    rep.classList.toggle('on', s.repeat !== 'off');
    rep.dataset.mode = s.repeat;
  }
}

function renderTime() {
  const pos = player.position;
  const dur = player.duration;
  if (!state.seeking) {
    $('#fp-seek').value = dur ? String(Math.round((pos / dur) * 1000)) : '0';
    $('#fp-cur').textContent = fmtTime(pos);
  }
  $('#fp-dur').textContent = fmtTime(dur);
  $('#mp-bar').style.width = dur ? `${(pos / dur) * 100}%` : '0';
  if ('mediaSession' in navigator && navigator.mediaSession.setPositionState && dur && player.playing) {
    try {
      navigator.mediaSession.setPositionState({ duration: dur, position: Math.min(pos, dur), playbackRate: 1 });
    } catch {
      /* ignore invalid state */
    }
  }
}

const seek = $('#fp-seek');
seek.addEventListener('input', () => {
  state.seeking = true;
  $('#fp-cur').textContent = fmtTime((Number(seek.value) / 1000) * player.duration);
});
seek.addEventListener('change', () => {
  player.seek((Number(seek.value) / 1000) * player.duration);
  state.seeking = false;
  seek.blur(); // a focused slider would swallow the keyboard shortcuts
});

// ------------------------------------------------------------------ navigation

$('#mini-open').addEventListener('click', () => showView('player'));

// Jump to the playing track in its list (szmy's tap-to-return cursor): from the player's
// title or the locate button.
async function locateCurrent() {
  const t = queue.current();
  if (!t) return;
  clearSearch(); // the track must be visible in its own list, not filtered out of it
  await showView(state.source);
  if (state.source === 'favorites') {
    const i = state.favTracks.findIndex((x) => x.path === t.path);
    if (i >= 0) list.scrollToIndex(i);
    return;
  }
  const dir = t.path.includes('/') ? t.path.slice(0, t.path.lastIndexOf('/')) : '';
  if (dir !== state.dir) await loadDir(dir);
  const i = state.entries.findIndex((x) => x.path === t.path);
  if (i >= 0) list.scrollToIndex(i);
}

$('#fp-title').addEventListener('click', () => state.meta && locateCurrent());
$('#btn-locate').addEventListener('click', locateCurrent);

// The search box: results update as you type (a short pause after each keystroke).
const applySearchSoon = debounce(applySearch, 120);
$('#search-input').addEventListener('input', () => {
  $('#search-clear').hidden = !$('#search-input').value;
  applySearchSoon();
});
$('#search-input').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') e.target.blur(); // closes the on-screen keyboard
  if (e.key === 'Escape') {
    e.stopPropagation();
    if ($('#search-input').value) {
      clearSearch();
      applySearch();
    } else {
      e.target.blur();
    }
  }
});
$('#search-clear').addEventListener('click', () => {
  clearSearch();
  applySearch();
  $('#search-input').focus();
});

// ------------------------------------------------------------------ buttons

const bind = (sel, fn) => $(sel).addEventListener('click', fn);
bind('#mp-play', () => player.toggle());
bind('#fp-play', () => player.toggle());
bind('#mp-next', () => advance(false));
bind('#fp-next', () => advance(false));
bind('#fp-prev', previous);
bind('#fp-shuffle', () => {
  state.settings.shuffle = !state.settings.shuffle;
  queue.setShuffle(state.settings.shuffle);
  saveSettings();
  renderTransport();
  toast(state.settings.shuffle ? 'Shuffle on' : 'Shuffle off', { ms: 1200 });
});
bind('#fp-repeat', () => {
  state.settings.repeat = queue.cycleRepeat();
  saveSettings();
  renderTransport();
  toast({ off: 'Repeat off', all: 'Repeat all', one: 'Repeat one' }[state.settings.repeat], { ms: 1200 });
});
bind('#fp-fav', () => state.meta && toggleFav({ path: state.meta.path, fav: state.meta.fav }));
bind('#fp-del', () => state.meta && deleteTrack({ path: state.meta.path, name: state.meta.title }));
bind('#btn-back', () => state.parent !== null && loadDir(state.parent));
document.querySelectorAll('.tab').forEach((t) => t.addEventListener('click', () => showView(t.dataset.view)));
bind('#np-browse', () => showView('library'));
bind('#np-upload', () => showView('upload'));

const vol = $('#fp-vol');
vol.addEventListener('input', () => {
  player.setVolume(Number(vol.value));
  state.settings.volume = player.volume;
  saveSettings();
});
vol.addEventListener('change', () => vol.blur());

// ------------------------------------------------------------------ visualizer

function ensureViz() {
  if (state.viz) return;
  player.ensureContext(); // the analyser only exists once the audio graph does
  state.viz = new Visualizer($('#fp-canvas'), player.analyser, player.ctx.sampleRate);
  state.viz.setMode(state.settings?.vizMode || 'bars');
}

function updateViz() {
  if (!state.viz) return;
  const visible = state.view === 'player' && player.playing;
  if (visible) state.viz.start();
  else state.viz.stop();
}

$('#fp-canvas').addEventListener('click', () => {
  const next = { bars: 'scope', scope: 'bars', off: 'bars' }[state.settings.vizMode];
  state.settings.vizMode = next;
  state.viz?.setMode(next);
  state.viz?.start();
  saveSettings();
});

// ------------------------------------------------------------------ media session, wake lock, resume

function updateMediaSession(meta) {
  if (!('mediaSession' in navigator)) return;
  navigator.mediaSession.metadata = new MediaMetadata({
    title: meta.title,
    artist: meta.artist || '',
    album: meta.album || '',
    artwork: [{ src: meta.hasArt ? api.artURL(meta.path) : '/icon.svg', sizes: '512x512' }],
  });
}

if ('mediaSession' in navigator) {
  const ms = navigator.mediaSession;
  const set = (name, fn) => {
    try {
      ms.setActionHandler(name, fn);
    } catch {
      /* action unsupported */
    }
  };
  set('play', () => player.play());
  set('pause', () => player.pause());
  set('previoustrack', guarded(previous));
  set('nexttrack', guarded(() => advance(false)));
  set('seekto', (d) => player.seek(d.seekTime));
  set('seekbackward', () => player.seek(player.position - 10));
  set('seekforward', () => player.seek(player.position + 10));
}

let wake = null;
async function updateWake() {
  try {
    if (player.playing && !wake && 'wakeLock' in navigator && document.visibilityState === 'visible') {
      wake = await navigator.wakeLock.request('screen');
      wake.addEventListener('release', () => (wake = null));
    } else if (!player.playing && wake) {
      await wake.release();
      wake = null;
    }
  } catch {
    wake = null;
  }
}
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') updateWake();
  else saveResume();
});

function saveResume() {
  const t = queue.current();
  if (!t || !state.started) return;
  api.putResume({ path: t.path, position: player.position, source: state.source }).catch(() => {});
}
setInterval(() => player.playing && saveResume(), 10000);
window.addEventListener('pagehide', saveResume);

// ------------------------------------------------------------------ settings

const saveSettings = debounce(() => api.putSettings(state.settings).catch((e) => toast(e.message)), 400);

function applySettings(s) {
  state.settings = s;
  queue.setShuffle(s.shuffle);
  queue.setRepeat(s.repeat);
  player.setVolume(s.volume);
  player.setLoopConfig({ mode: s.loopMode, count: s.loopCount, fade: s.fadeSeconds });
  state.viz?.setMode(s.vizMode);
  vol.value = String(s.volume);
  $('#set-loop').value = s.loopMode;
  $('#set-count').value = String(s.loopCount);
  $('#set-fade').value = String(s.fadeSeconds);
  $('#set-viz').value = s.vizMode;
  $('#set-guard').checked = s.skipGuard;
  renderTransport();
  renderLoopBadge();
}

function readSettingsForm() {
  const s = state.settings;
  s.loopMode = $('#set-loop').value;
  s.loopCount = Number($('#set-count').value) || 2;
  s.fadeSeconds = Number($('#set-fade').value) || 0;
  s.vizMode = $('#set-viz').value;
  s.skipGuard = $('#set-guard').checked;
  applySettings(s);
  saveSettings();
}
['#set-loop', '#set-count', '#set-fade', '#set-viz', '#set-guard'].forEach((id) => $(id).addEventListener('change', readSettingsForm));

bind('#btn-settings', async () => {
  $('#settings').hidden = false;
  $('#errlog').textContent = 'Loading…';
  try {
    const { lines } = await api.errors();
    $('#errlog').textContent = lines.length ? lines.join('\n') : 'No errors logged.';
  } catch (err) {
    $('#errlog').textContent = err.message;
  }
});
bind('#set-close', () => ($('#settings').hidden = true));
bind('#btn-logout', async () => {
  await api.logout().catch(() => {});
  location.reload();
});

// ------------------------------------------------------------------ upload

// Files dropped anywhere on the page go to the Upload screen (chunked and resumable, see uploadview.js).
window.addEventListener('dragover', (e) => e.preventDefault());
window.addEventListener('drop', (e) => {
  e.preventDefault();
  if (!e.dataTransfer?.files?.length || !state.uploadView) return;
  showView('upload').then(() => state.uploadView.addFiles([...e.dataTransfer.files]));
});

// ------------------------------------------------------------------ keyboard (desktop)

const KONAMI = ['ArrowUp', 'ArrowUp', 'ArrowDown', 'ArrowDown', 'ArrowLeft', 'ArrowRight', 'ArrowLeft', 'ArrowRight', 'b', 'a'];
let konami = 0;

document.addEventListener('keydown', (e) => {
  if (e.target.closest?.('input, select, textarea')) return;
  if (e.metaKey || e.ctrlKey || e.altKey) return;

  // Konami easter egg; while it is in progress its keys do nothing else.
  const key = e.key.length === 1 ? e.key.toLowerCase() : e.key;
  if (key === KONAMI[konami]) {
    konami++;
    if (konami > 2) e.preventDefault();
    if (konami === KONAMI.length) {
      konami = 0;
      toast('↑↑↓↓←→←→BA — 30 extra lives!', { ms: 3000 });
    }
    if (konami > 2) return;
  } else {
    konami = key === KONAMI[0] ? 1 : 0;
  }

  switch (e.key) {
    case ' ':
      e.preventDefault();
      player.toggle();
      break;
    case 'ArrowRight':
      e.shiftKey ? advance(false) : player.seek(player.position + 5);
      break;
    case 'ArrowLeft':
      e.shiftKey ? previous() : player.seek(player.position - 5);
      break;
    case 'ArrowUp':
      e.preventDefault();
      vol.value = String(Math.min(1, Number(vol.value) + 0.05));
      vol.dispatchEvent(new Event('input'));
      break;
    case 'ArrowDown':
      e.preventDefault();
      vol.value = String(Math.max(0, Number(vol.value) - 0.05));
      vol.dispatchEvent(new Event('input'));
      break;
    case 'n':
      advance(false);
      break;
    case 'p':
      previous();
      break;
    case 's':
      $('#fp-shuffle').click();
      break;
    case 'r':
      $('#fp-repeat').click();
      break;
    case 'f':
      $('#fp-fav').click();
      break;
    case 'l':
      locateCurrent();
      break;
    case '/':
      e.preventDefault(); // jump to the search box, on the library unless you are in favorites
      showView(state.view === 'favorites' ? 'favorites' : 'library').then(() => $('#search-input').focus());
      break;
    case 'Escape':
      $('#settings').hidden = true;
      break;
    default:
  }
});

// ------------------------------------------------------------------ login / boot

function showLogin() {
  $('#app').hidden = true;
  $('#login').hidden = false;
  $('#login-pw').focus();
}

$('#login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  $('#login-err').textContent = '';
  try {
    await api.login($('#login-pw').value);
    $('#login-pw').value = '';
    await startApp();
  } catch (err) {
    $('#login-err').textContent = err instanceof ApiError && err.status === 401 ? 'Wrong password' : err.message;
  }
});

events.addEventListener('unauthorized', () => state.started && showLogin());

async function startApp() {
  $('#login').hidden = true;
  $('#app').hidden = false;
  if (state.started) return reloadView();
  state.started = true;
  $('#btn-logout').hidden = !state.caps.authRequired; // nothing to sign out of in open-access mode
  state.uploadView = initUploadView({
    caps: state.caps,
    onUploaded: libraryChanged,
    goLibrary: () => showView('library'),
  });
  applySettings(await api.getSettings());
  showNowPlaying(null); // the app opens on the player, even with nothing to play
  await showView('player');
  await restoreResume();
}

async function restoreResume() {
  try {
    const r = await api.getResume();
    if (!r.path) return;
    if (r.source === 'favorites') {
      state.source = 'favorites';
      await loadFavorites();
    }
    const tracks = r.source === 'favorites' ? state.favTracks : await ensureLibTracks();
    const idx = tracks.findIndex((t) => t.path === r.path);
    if (idx < 0) return;
    queue.setQueue(tracks, idx);
    await startTrack(queue.current(), { autoplay: false, position: r.position, silent: true });
  } catch {
    /* resume is best-effort */
  }
}

async function boot() {
  if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => {});
  try {
    state.caps = await api.session();
  } catch (err) {
    document.body.textContent = `Cannot reach the server: ${err.message}`;
    return;
  }
  if (state.caps.authRequired && !state.caps.authenticated) showLogin();
  else await startApp();
}

boot();
