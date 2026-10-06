// Pop-out player: an always-on-top window (Document Picture-in-Picture) that stays visible while
// you work in other tabs or apps, like YouTube's mini player.
//
// The window mirrors the main player instead of holding its own state: each button forwards its
// click to the matching #fp-* control, and sync() / syncTime() copy their look back. So shuffle,
// repeat, favorites and the rest behave exactly as they do on the player screen.
//
// That needs Chrome or Edge 116+. Other browsers get pipvideo.js, which pops out a video of the
// cover with play/pause only.

import { $, setMarquee } from './util.js';
import * as fallback from './pipvideo.js';

const full = 'documentPictureInPicture' in window;
// Whether our own pop-out button can do anything (Firefox only has its built-in one on the video).
export const supported = full || document.pictureInPictureEnabled === true;

const BUTTONS = ['shuffle', 'prev', 'play', 'next', 'repeat', 'fav', 'talk'];

let win = null; // the pop-out window while it is open
let seeking = false;
let shownTitle = '';
let onChange = () => {};

const q = (sel) => win?.document.querySelector(sel);

const MARKUP = `
<div class="pip">
  <img id="pip-art" class="pip-art" alt="">
  <div class="pip-info">
    <div id="pip-title" class="ticker pip-title"></div>
    <div id="pip-sub" class="sub"></div>
  </div>
  <div class="seekrow">
    <span id="pip-cur" class="time">0:00</span>
    <input id="pip-seek" type="range" min="0" max="1000" value="0" aria-label="Seek">
    <span id="pip-dur" class="time">0:00</span>
  </div>
  <div class="controls">
    <button id="pip-shuffle" class="ib"></button>
    <button id="pip-prev" class="ib"></button>
    <button id="pip-play" class="ib play"></button>
    <button id="pip-next" class="ib"></button>
    <button id="pip-repeat" class="ib rep"></button>
  </div>
  <div class="controls second">
    <button id="pip-fav" class="ib fav"></button>
    <button id="pip-talk" class="ib talk"></button>
    <input id="pip-vol" type="range" min="0" max="1" step="0.01" aria-label="Volume">
  </div>
</div>`;

export const isOpen = () => (full ? !!win : fallback.isOpen());

// Sets things up. onToggle is called with true / false whenever the pop-out opens or closes.
export function init(player, onToggle) {
  onChange = onToggle;
  if (!full) fallback.init(player, onToggle);
}

export async function toggle() {
  if (isOpen()) return close();
  return open();
}

export async function close() {
  if (!full) return fallback.close();
  win?.close();
}

export async function open() {
  if (!full) return fallback.open();
  if (win) return;
  const w = await window.documentPictureInPicture.requestWindow({ width: 360, height: 260 });
  win = w;
  const doc = w.document;
  doc.title = 'Master Music Player';
  doc.documentElement.style.colorScheme = 'dark light';

  // Same-origin stylesheets are copied rule by rule so there is no unstyled flash.
  for (const sheet of document.styleSheets) {
    try {
      const style = doc.createElement('style');
      style.textContent = [...sheet.cssRules].map((r) => r.cssText).join('\n');
      doc.head.append(style);
    } catch {
      const link = doc.createElement('link');
      link.rel = 'stylesheet';
      link.href = sheet.href;
      doc.head.append(link);
    }
  }
  // The icons are <use href="#i-..."> references, so the sprite has to live in this document too.
  doc.body.append($('svg').cloneNode(true));
  doc.body.insertAdjacentHTML('beforeend', MARKUP);

  for (const name of BUTTONS) q(`#pip-${name}`).addEventListener('click', () => $(`#fp-${name}`).click());

  const seek = q('#pip-seek');
  seek.addEventListener('input', () => {
    seeking = true;
    const main = $('#fp-seek');
    main.value = seek.value;
    main.dispatchEvent(new Event('input')); // the main slider works out the time label
    q('#pip-cur').textContent = $('#fp-cur').textContent;
  });
  seek.addEventListener('change', () => {
    const main = $('#fp-seek');
    main.value = seek.value;
    main.dispatchEvent(new Event('change'));
    seeking = false;
    seek.blur();
  });

  const vol = q('#pip-vol');
  vol.addEventListener('input', () => {
    const main = $('#fp-vol');
    main.value = vol.value;
    main.dispatchEvent(new Event('input'));
  });
  vol.addEventListener('change', () => vol.blur());

  // Keyboard shortcuts typed in the pop-out go to the main page's handler.
  doc.addEventListener('keydown', (e) => {
    if (e.target.closest?.('input')) return;
    const fwd = new KeyboardEvent('keydown', {
      key: e.key, code: e.code, shiftKey: e.shiftKey, ctrlKey: e.ctrlKey, altKey: e.altKey, metaKey: e.metaKey, cancelable: true,
    });
    document.dispatchEvent(fwd);
    if (fwd.defaultPrevented) e.preventDefault();
  });

  w.addEventListener('pagehide', () => {
    if (win !== w) return;
    win = null;
    shownTitle = '';
    seeking = false;
    onChange(false);
  });

  sync();
  onChange(true);
}

// Copies everything from the main player: track, buttons and time.
export function sync() {
  if (!full) return fallback.sync();
  if (!win) return;
  const art = $('#fp-art').src; // the property is an absolute URL, which the pop-out needs
  if (q('#pip-art').src !== art) q('#pip-art').src = art;
  const title = $('#fp-title').textContent;
  if (title !== shownTitle) {
    shownTitle = title;
    setMarquee(q('#pip-title'), title);
    win.document.title = title;
  }
  q('#pip-sub').textContent = $('#fp-artist').textContent;
  for (const name of BUTTONS) {
    const src = $(`#fp-${name}`);
    const dst = q(`#pip-${name}`);
    if (dst.innerHTML !== src.innerHTML) dst.innerHTML = src.innerHTML;
    dst.className = src.className;
    dst.disabled = src.disabled;
    dst.setAttribute('aria-label', src.getAttribute('aria-label') || '');
    if (src.dataset.mode) dst.dataset.mode = src.dataset.mode;
  }
  q('#pip-seek').disabled = $('#fp-seek').disabled;
  syncTime();
}

// The cheap part of sync(), run on every time update.
export function syncTime() {
  if (!full) return fallback.sync();
  if (!win) return;
  if (!seeking) {
    q('#pip-seek').value = $('#fp-seek').value;
    q('#pip-cur').textContent = $('#fp-cur').textContent;
  }
  q('#pip-dur').textContent = $('#fp-dur').textContent;
  q('#pip-vol').value = $('#fp-vol').value;
}
