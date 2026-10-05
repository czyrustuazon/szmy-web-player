export const $ = (sel, root = document) => root.querySelector(sel);

export function fmtTime(sec) {
  if (!Number.isFinite(sec) || sec < 0) return '0:00';
  const s = Math.floor(sec);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const r = String(s % 60).padStart(2, '0');
  return h ? `${h}:${String(m).padStart(2, '0')}:${r}` : `${m}:${r}`;
}

export function fmtBytes(n) {
  if (!Number.isFinite(n) || n < 0) return '';
  if (n >= 1024 ** 3) return `${(n / 1024 ** 3).toFixed(2)} GB`;
  if (n >= 1024 ** 2) return `${(n / 1024 ** 2).toFixed(1)} MB`;
  if (n >= 1024) return `${Math.round(n / 1024)} KB`;
  return `${n} B`;
}

export function icon(name) {
  return `<svg class="ic" aria-hidden="true"><use href="#i-${name}"/></svg>`;
}

export function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
}

let toastTimer = 0;
// Toast with an optional action button (used for Undo).
export function toast(message, { action, onAction, ms = 3500 } = {}) {
  const box = $('#toast');
  box.innerHTML = '';
  const span = document.createElement('span');
  span.textContent = message;
  box.append(span);
  if (action) {
    const b = document.createElement('button');
    b.textContent = action;
    b.addEventListener('click', () => {
      box.hidden = true;
      onAction?.();
    });
    box.append(b);
  }
  box.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (box.hidden = true), ms);
}

// Scroll long titles horizontally when they overflow, like szmy's tag ticker.
export function setMarquee(el, text) {
  el.textContent = text;
  el.classList.remove('marquee');
  el.style.removeProperty('--shift');
  requestAnimationFrame(() => {
    const overflow = el.scrollWidth - el.clientWidth;
    if (overflow > 4) {
      el.style.setProperty('--shift', `-${overflow + 16}px`);
      el.style.setProperty('--dur', `${Math.max(6, overflow / 25)}s`);
      el.classList.add('marquee');
    }
  });
}

export function debounce(fn, ms) {
  let t = 0;
  return (...args) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...args), ms);
  };
}

// What to call a file's format in the UI. vgmstream and ffmpeg cover dozens of formats, so for
// them the file extension (WMA, BRSTM...) says more than the kind does.
export function kindLabel(kind, path = '', fallback = 'file') {
  if (kind === 'vgm' || kind === 'ffmpeg') {
    const ext = /\.([A-Za-z0-9]{1,6})$/.exec(path || '');
    if (ext) return ext[1].toUpperCase();
  }
  return (kind || fallback).toUpperCase();
}
