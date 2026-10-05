// End-to-end check of chunked, resumable uploads: the real browser upload client
// (web/js/uploader.js) against a real running server, with real zip and 7z archives.
// Run through scripts/e2e.sh (it builds the image, makes fixtures and calls this).
//
// Not covered: the XHR transport the browser uses for chunks (Node has no XMLHttpRequest),
// so chunks go through fetch here. Everything else is the shipped code path.

import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { createUploader, UploadError } from './js/uploader.js';

const BASE = process.env.E2E_URL || 'http://127.0.0.1:18199';
const FIX = '/e2e/fixtures';
const MUSIC = '/e2e/music';

const H = { 'X-Requested-With': 'masterplayer' };
const realTransport = {
  async json(method, url, body) {
    const res = await fetch(BASE + url, {
      method,
      headers: body === undefined ? H : { ...H, 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const data = await res.json().catch(() => null);
    if (!res.ok) throw new UploadError((data && data.error) || res.statusText, res.status);
    return data;
  },
  async chunk(url, headers, blob) {
    const res = await fetch(BASE + url, { method: 'POST', headers: { ...H, ...headers }, body: Buffer.from(await blob.arrayBuffer()) });
    return { status: res.status, body: await res.json().catch(() => null) };
  },
};

let failures = 0;
const check = (cond, msg) => {
  console.log(`${cond ? 'ok  ' : 'FAIL'} ${msg}`);
  if (!cond) failures++;
};
const sha = (buf) => crypto.createHash('sha256').update(buf).digest('hex');
const fileOf = (name) => new File([fs.readFileSync(path.join(FIX, name))], name);
const memoryStorage = () => {
  const m = new Map();
  return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => m.set(k, v), removeItem: (k) => m.delete(k) };
};
const walk = (dir) =>
  fs.existsSync(dir)
    ? fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? walk(path.join(dir, e.name)) : [path.join(dir, e.name)]))
    : [];

// ---- 0. the folders were created by Docker as root: uploads must work anyway
{
  const sess = await (await fetch(`${BASE}/api/session`)).json();
  check(sess.canUpload === true, 'uploads are enabled even though Docker created the folders as root');
  check(sess.canDelete === false, 'a root-owned library root only turns deleting off');
}

// ---- 1. a 40 MiB file (3 chunks at the real 16 MiB size) that loses its connection, then resumes
{
  const storage = memoryStorage();
  let chunkCalls = 0;
  let outage = true;
  const flaky = {
    json: realTransport.json,
    async chunk(url, headers, blob, progress) {
      chunkCalls++;
      if (outage && chunkCalls >= 2) throw new UploadError('network down'); // chunk 1 lands, then the line dies
      return realTransport.chunk(url, headers, blob, progress);
    },
  };
  const big = fileOf('big.mp3');
  const first = createUploader({ transport: flaky, storage, retryBaseMs: 1 });
  let interrupted = false;
  try {
    await first.uploadBatch([big], { title: 'Big' });
  } catch {
    interrupted = true;
  }
  check(interrupted, 'upload is interrupted when the connection drops (and gives up after bounded retries)');
  check(first.pending() !== null, 'the interrupted batch is remembered');

  outage = false;
  chunkCalls = 0;
  const resumed = createUploader({ transport: flaky, storage }); // a "reload": new uploader, same storage
  const [r] = await resumed.uploadBatch([big], { title: 'Big' });
  check(r.state === 'done', 'resuming completes the upload');
  check(chunkCalls === 2, `only the missing chunks were sent after the resume (${chunkCalls} of 3)`);
  const onDisk = path.join(MUSIC, 'uploads', 'Big', 'big.mp3');
  check(fs.existsSync(onDisk) && sha(fs.readFileSync(onDisk)) === sha(fs.readFileSync(path.join(FIX, 'big.mp3'))), 'the file arrived byte for byte');
}

// ---- 2. archives: only audio survives, symlinks and junk are dropped, a wrapper folder is removed
for (const [archive, title] of [['album.zip', 'Zip Album'], ['album.7z', '7z Album']]) {
  const messages = [];
  const up = createUploader({ transport: realTransport, storage: memoryStorage(), pollMs: 200 });
  const [r] = await up.uploadBatch([fileOf(archive)], { title, onStatus: (m) => messages.push(m) });
  check(r.state === 'done', `${archive}: finished (${r.error || 'ok'})`);
  check(r.tracks === 3, `${archive}: three tracks added (got ${r.tracks})`);
  const dir = path.join(MUSIC, 'uploads', title);
  const names = walk(dir).map((p) => path.relative(dir, p)).sort();
  check(JSON.stringify(names) === JSON.stringify(['01.mp3', '02.mp3', 'CD2/03.mp3', 'cover.jpg']), `${archive}: the audio files and the real cover picture, wrapper folder removed (${names.join(', ')})`);
  // The 7z extractor materialises the symlink (and it is dropped); the zip reader never writes one.
  const wantTypes = archive.endsWith('.7z') ? { txt: 1, sh: 1, png: 1, mp3: 1 } : { txt: 1, sh: 1, png: 1 };
  const wantSkipped = Object.values(wantTypes).reduce((a, b) => a + b, 0);
  check(r.images === 1 && r.skipped === wantSkipped, `${archive}: one picture kept, ${wantSkipped} files skipped (${r.images}, ${r.skipped})`);
  const types = r.skippedTypes || {};
  check(JSON.stringify(Object.entries(types).sort()) === JSON.stringify(Object.entries(wantTypes).sort()), `${archive}: skipped files are broken down by type (${JSON.stringify(types)})`);
  check(r.hasReport === true, `${archive}: a report of the skipped files was saved`);
  const report = await fetch(`${BASE}/api/upload/report?relPath=${encodeURIComponent(r.path)}&filename=${encodeURIComponent(archive)}`);
  const reportText = await report.text();
  check(report.status === 200 && /text\/plain/.test(report.headers.get('content-type')) && reportText.includes('readme.txt') && reportText.includes('run.sh') && !reportText.includes('cover.jpg'), `${archive}: the report lists what was left out`);
  const tracksOfAlbum = (await (await fetch(`${BASE}/api/tracks?dir=${encodeURIComponent('uploads/' + title)}`)).json()).tracks;
  const art = await fetch(`${BASE}/api/art?p=${encodeURIComponent(tracksOfAlbum[0].path)}`);
  check(art.status === 200 && art.headers.get('content-type') === 'image/jpeg', `${archive}: the folder picture is the cover of its tracks (${art.status})`);
  check(!tracksOfAlbum.some((t) => t.path.endsWith('.jpg')) && !(await (await fetch(`${BASE}/api/browse?dir=${encodeURIComponent('uploads/' + title)}`)).text()).includes('cover.jpg'), `${archive}: pictures are not listed as library files`);
  check(walk(dir).every((p) => !fs.lstatSync(p).isSymbolicLink()), `${archive}: no symlinks`);
  check(fs.readdirSync(dir).every((n) => !n.startsWith('.')), `${archive}: no scratch folders left behind`);
}

// ---- 3. a loose file that is not audio is refused and never stored
{
  const up = createUploader({ transport: realTransport, storage: memoryStorage() });
  const [r] = await up.uploadBatch([new File([Buffer.from('definitely not music')], 'notes.txt')], { title: 'Junk' });
  check(r.state === 'failed' && /not an audio file/.test(r.error), `a text file is refused (${r.error})`);
  check(walk(path.join(MUSIC, 'uploads', 'Junk')).length === 0, 'and nothing is stored');
}

// ---- 3b. formats a browser cannot play are converted by ffmpeg to FLAC and stream with ranges
{
  const sess = await (await fetch(`${BASE}/api/session`)).json();
  check(sess.ffmpeg === true, 'the server found ffmpeg');
  const names = ['song.wma', 'song.wv', 'song.mka', 'song.flv'];
  const up = createUploader({ transport: realTransport, storage: memoryStorage() });
  const results = await up.uploadBatch(names.map((n) => new File([fs.readFileSync(path.join(FIX, 'ff', n))], n)), { title: 'Converted' });
  check(results.every((r) => r.state === 'done'), `ffmpeg formats are accepted as audio (${results.map((r) => r.state + (r.error ? ': ' + r.error : '')).join(', ')})`);
  for (const n of names) {
    const p = encodeURIComponent(`uploads/Converted/${n}`);
    const meta = await (await fetch(`${BASE}/api/meta?p=${p}`)).json();
    check(meta.kind === 'ffmpeg' && meta.native === false && meta.sampleRate > 0, `${n}: detected and probed (${meta.kind}, ${meta.sampleRate} Hz)`);
    if (n !== 'song.flv') check(meta.title === 'Windows' && meta.artist === 'Redmond', `${n}: tags come from ffprobe (${meta.title} / ${meta.artist})`);
    const res = await fetch(`${BASE}/api/stream?p=${p}`);
    const body = Buffer.from(await res.arrayBuffer());
    check(res.status === 200 && res.headers.get('content-type') === 'audio/flac' && body.subarray(0, 4).toString() === 'fLaC', `${n}: streamed as FLAC (${res.status}, ${res.headers.get('content-type')})`);
    const part = await fetch(`${BASE}/api/stream?p=${p}`, { headers: { Range: 'bytes=0-3' } });
    check(part.status === 206 && (await part.text()) === 'fLaC', `${n}: the converted file supports range requests`);
  }
}

// ---- 4. nothing is left staged, and the uploads are really in the library
{
  check(walk(path.join(MUSIC, 'uploads', '.uploads')).filter((p) => !p.includes('/reports/')).length === 0, 'the staging area holds nothing but the skipped-file reports');
  check(fs.existsSync('/e2e/uploads-host/.uploads'), 'staging lives inside the uploads folder (the same disk as the result)');
  check(!fs.existsSync(path.join(MUSIC, '.uploads')), 'nothing is staged on the library disk');
  const hostCopy = '/e2e/uploads-host/Big/big.mp3';
  check(fs.existsSync(hostCopy) && sha(fs.readFileSync(hostCopy)) === sha(fs.readFileSync(path.join(FIX, 'big.mp3'))), 'the file is in the uploads folder on the host');
  const res = await fetch(`${BASE}/api/tracks`);
  const { tracks } = await res.json();
  check(tracks.length === 11, `the library lists 11 tracks (got ${tracks.length})`);
  const first = tracks.find((t) => t.path.endsWith('big.mp3'));
  const meta = await (await fetch(`${BASE}/api/meta?p=${encodeURIComponent(first.path)}`)).json();
  check(meta.kind === 'mp3', 'an uploaded file can be opened');
  const range = await fetch(`${BASE}/api/stream?p=${encodeURIComponent(first.path)}`, { headers: { Range: 'bytes=0-9' } });
  check(range.status === 206, 'and streamed with range requests');
}

console.log(failures ? `\nE2E FAILED (${failures})` : '\nE2E PASSED');
process.exit(failures ? 1 : 0);
