// The Upload screen: pick files (or drop them), optionally name a folder, and send
// them with the chunked, resumable uploader. Archives (.zip / .7z) are unpacked on
// the server and reduced to their audio files.

import { createUploader, batchSignature } from './uploader.js';
import { $, toast, escapeHTML, fmtBytes } from './util.js';
import { events } from './api.js';

export const isArchive = (name) => /\.(zip|7z)$/i.test(name);
export const is7z = (name) => /\.7z$/i.test(name);
const stem = (name) => name.replace(/\.[^.]+$/, '');

// Where a batch lands. A typed name wins. Otherwise one lone archive gets a folder named
// after itself (so an album does not spill into the shared upload folder); loose files go
// straight into the upload folder.
export function suggestTitle(files, typed = '') {
  const t = typed.trim();
  if (t) return t;
  return files.length === 1 && isArchive(files[0].name) ? stem(files[0].name) : '';
}

// "jpg ×927, txt ×66, other ×3": the biggest groups of skipped files, then a tail count.
export function describeTypes(types = {}, max = 6) {
  const rows = Object.entries(types).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
  const shown = rows.slice(0, max).map(([t, n]) => `${t} ×${n}`);
  if (rows.length > max) shown.push(`${rows.length - max} more type${rows.length - max === 1 ? '' : 's'}`);
  return shown.join(', ');
}

// Where the full list of skipped files can be read, or '' when there is none.
export function reportUrl(r) {
  if (!r.hasReport) return '';
  return `/api/upload/report?relPath=${encodeURIComponent(r.path || '')}&filename=${encodeURIComponent(r.name)}`;
}

// One line of the results list for a finished file.
export function describeResult(r) {
  if (r.state === 'done') {
    const parts = [];
    if (r.duplicates > 0) {
      const dup = `${r.duplicates} already in the library`;
      if (!r.tracks) return r.skipped > 0 ? `Nothing new: ${dup}, ${r.skipped} other file${r.skipped === 1 ? '' : 's'} skipped` : `Nothing new: ${dup}`;
      parts.push(dup);
    }
    if (r.skipped !== undefined && r.skipped > 0) {
      const what = describeTypes(r.skippedTypes);
      parts.push(`${r.skipped} other file${r.skipped === 1 ? '' : 's'} skipped${what ? ` (${what})` : ''}`);
    }
    if (parts.length) return `${r.tracks} track${r.tracks === 1 ? '' : 's'} added, ${parts.join(', ')}`;
    return r.tracks > 1 ? `${r.tracks} tracks added` : 'Added to the library';
  }
  return r.error || 'Upload failed';
}

export function summarize(results) {
  const ok = results.filter((r) => r.state === 'done');
  const tracks = ok.reduce((n, r) => n + (r.tracks || 0), 0);
  const dups = ok.reduce((n, r) => n + (r.duplicates || 0), 0);
  const bad = results.length - ok.length;
  if (!ok.length) return { tracks: 0, bad, text: `Nothing was added (${bad} file${bad === 1 ? '' : 's'} failed)` };
  const base = tracks || !dups ? `Added ${tracks} track${tracks === 1 ? '' : 's'}${dups ? `, ${dups} already there` : ''}` : `Nothing new: ${dups} already in the library`;
  return { tracks, bad, text: bad ? `${base}; ${bad} failed` : base };
}

export function initUploadView({ caps, onUploaded, goLibrary, uploader = createUploader() }) {
  const el = {
    drop: $('#up-drop'),
    input: $('#up-input'),
    folder: $('#up-folder'),
    merge: $('#up-merge'),
    files: $('#up-files'),
    start: $('#up-start'),
    clear: $('#up-clear'),
    progress: $('#up-progress'),
    bar: $('#up-bar'),
    status: $('#up-status'),
    results: $('#up-results'),
    resume: $('#up-resume'),
    note7z: $('#up-7z'),
    disabled: $('#up-disabled'),
  };
  const enabled = !!caps.canUpload;
  let queue = [];
  let busy = false;
  let wake = null;
  let interrupted = false;

  el.disabled.hidden = enabled;
  el.drop.disabled = !enabled;
  el.folder.disabled = !enabled;
  el.merge.disabled = !enabled;

  function setStatus(text) {
    el.status.textContent = text;
  }

  function render() {
    el.files.innerHTML = '';
    queue.forEach((f, i) => {
      const li = document.createElement('li');
      const kind = isArchive(f.name) ? '<span class="tag">archive</span>' : '';
      li.innerHTML = `${kind}<span class="nm">${escapeHTML(f.name)}</span><span class="sz">${fmtBytes(f.size)}</span><button class="ib rm" aria-label="Remove ${escapeHTML(f.name)}" data-i="${i}" ${busy ? 'disabled' : ''}>&times;</button>`;
      el.files.append(li);
    });
    el.start.disabled = !enabled || busy || !queue.length;
    el.start.textContent = interrupted ? 'Resume' : 'Upload';
    el.clear.disabled = busy || !queue.length;
    el.folder.placeholder = suggestTitle(queue) || 'Album or collection name';
    el.note7z.hidden = caps.sevenZip !== false || !queue.some((f) => is7z(f.name));
    showResumeHint();
  }

  function showResumeHint() {
    const pending = uploader.pending();
    if (!pending || busy) {
      el.resume.hidden = true;
      return;
    }
    const matches = queue.length > 0 && batchSignature(queue) === pending.sig;
    el.resume.hidden = false;
    el.resume.textContent = matches
      ? 'These files match an interrupted upload. Press Upload to continue where it stopped.'
      : `An earlier upload was interrupted (${(pending.names || []).join(', ') || 'unknown files'}). Choose the same files again to continue it.`;
  }

  function addFiles(list) {
    if (!enabled || busy) return;
    let added = 0;
    for (const f of list) {
      if (f.size === 0) {
        toast(`${f.name} is empty and was skipped`);
        continue;
      }
      if (queue.some((q) => q.name === f.name && q.size === f.size)) continue;
      queue.push(f);
      added++;
    }
    const pending = uploader.pending();
    if (pending && !el.folder.value.trim() && batchSignature(queue) === pending.sig) el.folder.value = pending.title || '';
    if (added) el.results.innerHTML = '';
    render();
  }

  async function acquireWake() {
    try {
      if ('wakeLock' in navigator) wake = await navigator.wakeLock.request('screen');
    } catch {
      wake = null;
    }
  }

  function showResults(results) {
    el.results.innerHTML = '';
    for (const r of results) {
      const li = document.createElement('li');
      li.className = r.state === 'done' ? 'ok' : 'bad';
      const report = reportUrl(r);
      const link = report ? ` <a href="${escapeHTML(report)}" target="_blank" rel="noopener">See the list</a>` : '';
      li.innerHTML = `<span>${r.state === 'done' ? '✓' : '✗'}</span><span class="nm">${escapeHTML(r.name)}<span class="why">${escapeHTML(describeResult(r))}${link}</span></span>`;
      el.results.append(li);
    }
  }

  async function start() {
    if (busy || !queue.length || !enabled) return;
    busy = true;
    interrupted = false;
    el.results.innerHTML = '';
    el.progress.hidden = false;
    el.bar.style.width = '0%';
    render();
    acquireWake();
    const title = suggestTitle(queue, el.folder.value);
    try {
      const results = await uploader.uploadBatch(queue, {
        title,
        merge: el.merge.checked && title !== '',
        onStatus: setStatus,
        onProgress: (p) => {
          el.bar.style.width = `${Math.round(p.fraction * 100)}%`;
          if (p.extracting === undefined) setStatus(`Uploading ${p.file}… ${Math.round(p.fraction * 100)}%`);
        },
      });
      showResults(results);
      const sum = summarize(results);
      setStatus(sum.text);
      queue = queue.filter((f) => !results.some((r) => r.name === f.name && r.state === 'done'));
      if (sum.tracks) {
        onUploaded();
        toast(sum.text, { action: 'Open library', onAction: goLibrary, ms: 7000 });
      } else {
        toast(sum.text, { ms: 7000 });
      }
    } catch (err) {
      interrupted = true;
      if (err.status === 401) events.dispatchEvent(new Event('unauthorized')); // show the sign-in screen
      setStatus(`Upload interrupted: ${err.message}. Press Resume to continue where it stopped.`);
      toast('Upload interrupted. It can be resumed.', { ms: 6000 });
    } finally {
      busy = false;
      el.progress.hidden = true;
      try {
        await wake?.release();
      } catch {
        /* already released */
      }
      wake = null;
      render();
    }
  }

  el.drop.addEventListener('click', () => el.input.click());
  el.input.addEventListener('change', () => {
    addFiles([...el.input.files]);
    el.input.value = '';
  });
  ['dragenter', 'dragover'].forEach((t) =>
    el.drop.addEventListener(t, (e) => {
      e.preventDefault();
      el.drop.classList.add('over');
    }),
  );
  ['dragleave', 'drop'].forEach((t) => el.drop.addEventListener(t, () => el.drop.classList.remove('over')));
  el.drop.addEventListener('drop', (e) => {
    e.preventDefault();
    addFiles([...(e.dataTransfer?.files || [])]);
  });
  el.files.addEventListener('click', (e) => {
    const i = e.target.closest('button')?.dataset.i;
    if (i === undefined || busy) return;
    queue.splice(Number(i), 1);
    render();
  });
  el.start.addEventListener('click', start);
  el.clear.addEventListener('click', () => {
    if (busy) return;
    queue = [];
    interrupted = false;
    setStatus('');
    el.results.innerHTML = '';
    render();
  });
  window.addEventListener('beforeunload', (e) => {
    if (busy) {
      e.preventDefault();
      e.returnValue = ''; // the upload can resume, but warn before leaving
    }
  });
  render();

  return { addFiles, show: render };
}
