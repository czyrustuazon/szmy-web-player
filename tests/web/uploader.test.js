import test from 'node:test';
import assert from 'node:assert/strict';
import { createUploader, crc32Bytes, hashChunk, UploadError, batchSignature, MAX_CHUNK_RETRIES } from '../../web/js/uploader.js';

// ------------------------------------------------------------------ an in-memory server speaking the same protocol

class FakeServer {
  constructor() {
    this.sessions = new Map(); // "rel|name" -> {size, bytes}
    this.done = new Map(); // "rel|name" -> status
    this.calls = [];
    this.starts = 0;
    this.hooks = {}; // failure injection
    this.polls = 0;
    this.archivePolls = 2; // how many "running" answers an archive gets
  }

  key(rel, name) {
    return `${rel}|${name}`;
  }

  async json(method, url, body) {
    this.calls.push(`${method} ${url.split('?')[0]}`);
    const u = new URL(url, 'http://x');
    if (this.hooks.json) await this.hooks.json(method, u.pathname);
    switch (u.pathname) {
      case '/api/upload/start':
        this.starts++;
        return { name: body.title, relPath: `uploads/${body.title || ''}`.replace(/\/$/, '') };
      case '/api/upload/begin': {
        if (this.hooks.refuse) this.hooks.refuse(body);
        const k = this.key(body.relPath, body.filename);
        let s = this.sessions.get(k);
        if (!s || s.size !== body.size) {
          s = { size: body.size, bytes: new Uint8Array(0) };
          this.sessions.set(k, s);
        }
        this.done.delete(k);
        return { offset: s.bytes.length };
      }
      case '/api/upload/complete': {
        const k = this.key(body.relPath, body.filename);
        if (this.done.has(k)) return this.done.get(k);
        const s = this.sessions.get(k);
        if (this.hooks.corruptOnce && s) {
          const rewind = this.hooks.corruptOnce;
          this.hooks.corruptOnce = null;
          s.bytes = s.bytes.slice(0, rewind);
          return { state: 'corrupted', resumeOffset: rewind, error: 'damaged', totalBytes: s.size, bytesWritten: 0 };
        }
        this.finalBytes = this.finalBytes || {};
        this.finalBytes[body.filename] = s.bytes;
        if (/\.(zip|7z)$/.test(body.filename)) {
          this.polls = 0;
          this.done.set(k, { state: 'running', totalBytes: s.size, bytesWritten: 0 });
          return this.done.get(k);
        }
        const st = { state: 'done', totalBytes: s.size, bytesWritten: s.size, tracks: 1, path: `${body.relPath}/${body.filename}` };
        this.done.set(k, st);
        return st;
      }
      case '/api/upload/status': {
        const k = this.key(u.searchParams.get('relPath'), u.searchParams.get('filename'));
        if (this.hooks.lose) {
          this.hooks.lose = false;
          this.done.delete(k);
        }
        const st = this.done.get(k);
        if (!st) throw new UploadError('nothing tracked', 404);
        if (st.state === 'running' && ++this.polls > this.archivePolls) {
          const fin = { state: this.hooks.archiveFails ? 'failed' : 'done', totalBytes: st.totalBytes, bytesWritten: st.totalBytes, tracks: 12, skipped: 3, error: this.hooks.archiveFails };
          this.done.set(k, fin);
          return fin;
        }
        return { ...st, bytesWritten: Math.floor(st.totalBytes / 2) };
      }
    }
    throw new Error(`unexpected ${method} ${url}`);
  }

  async chunk(url, headers, blob, onProgress) {
    this.calls.push('CHUNK');
    const u = new URL(url, 'http://x');
    const rel = u.searchParams.get('relPath');
    const name = u.searchParams.get('filename');
    const offset = Number(u.searchParams.get('offset'));
    const n = (this.chunkNo = (this.chunkNo || 0) + 1);
    if (this.hooks.beforeChunk) this.hooks.beforeChunk(n);
    const s = this.sessions.get(this.key(rel, name));
    const data = new Uint8Array(await blob.arrayBuffer());
    onProgress?.(data.length);
    if (headers['X-Chunk-CRC32'] !== crc32Bytes(data)) return { status: 400, body: { error: 'bad crc' } };
    if (s.bytes.length !== offset) return { status: 409, body: { offset: s.bytes.length } };
    const next = new Uint8Array(s.bytes.length + data.length);
    next.set(s.bytes);
    next.set(data, s.bytes.length);
    s.bytes = next;
    // Failure injection: the chunk LANDED but the response is lost.
    if (this.hooks.dropResponseAfter === n) throw new UploadError('connection reset');
    if (this.hooks.status && this.hooks.status[n]) return { status: this.hooks.status[n], body: { error: 'nope' } };
    return { status: 200, body: { offset: s.bytes.length } };
  }
}

const file = (name, size, fill = 7) => new File([new Uint8Array(size).fill(fill)], name);
const noSleep = async () => {};

function uploaderFor(server, extra = {}) {
  const mem = new Map();
  const storage = { getItem: (k) => mem.get(k) ?? null, setItem: (k, v) => mem.set(k, v), removeItem: (k) => mem.delete(k) };
  return createUploader({ transport: server, sleep: noSleep, chunkSize: 100, storage, ...extra });
}

// ------------------------------------------------------------------ tests

test('crc32 matches the standard check value and the server-side algorithm', async () => {
  assert.equal(crc32Bytes(new TextEncoder().encode('123456789')), 'cbf43926');
  assert.equal(crc32Bytes(new Uint8Array(0)), '00000000');
  assert.equal(await hashChunk(new Blob([new TextEncoder().encode('123456789')])), 'cbf43926');
});

test('a file is sent in chunks and arrives byte for byte', async () => {
  const server = new FakeServer();
  const payload = Uint8Array.from({ length: 450 }, (_, i) => i % 251);
  const f = new File([payload], 'song.mp3');
  const progress = [];
  const results = await uploaderFor(server).uploadBatch([f], { title: 'Singles', onProgress: (p) => progress.push(p.fraction) });
  assert.equal(results.length, 1);
  assert.equal(results[0].state, 'done');
  assert.equal(results[0].name, 'song.mp3');
  assert.deepEqual([...server.finalBytes['song.mp3']], [...payload]);
  assert.equal(server.calls.filter((c) => c === 'CHUNK').length, 5); // 450 bytes / 100
  assert.equal(server.starts, 1);
  assert.ok(progress.length > 0 && progress.every((p) => p >= 0 && p <= 1));
  assert.equal(Math.max(...progress), 1);
});

test('an empty file still completes (no chunks needed)', async () => {
  const server = new FakeServer();
  const [r] = await uploaderFor(server).uploadBatch([file('empty.mp3', 0)], { title: 't' });
  assert.equal(r.state, 'done');
  assert.equal(server.calls.filter((c) => c === 'CHUNK').length, 0);
});

test('a dropped connection is retried from the offset the server reports', async () => {
  const server = new FakeServer();
  server.hooks.dropResponseAfter = 2; // chunk 2 LANDS on the server but the client never hears back
  const payload = Uint8Array.from({ length: 350 }, (_, i) => (i * 7) % 256);
  const messages = [];
  const [r] = await uploaderFor(server).uploadBatch([new File([payload], 'a.mp3')], { title: 't', onStatus: (m) => messages.push(m) });
  assert.equal(r.state, 'done');
  // The crucial property: no duplicated or misaligned bytes, even though a chunk landed unacknowledged.
  assert.deepEqual([...server.finalBytes['a.mp3']], [...payload]);
  assert.ok(messages.some((m) => m.includes('retrying')));
});

test('a stale offset (409) just resynchronises', async () => {
  const server = new FakeServer();
  const f = file('a.mp3', 300);
  // Pretend another tab already delivered the first 100 bytes.
  await server.json('POST', '/api/upload/begin', { relPath: 'uploads/t', filename: 'a.mp3', size: 300 });
  server.sessions.get('uploads/t|a.mp3').bytes = new Uint8Array(100).fill(7);
  const rec = { sig: batchSignature([f]), title: 't', relPath: 'uploads/t' };
  const u = uploaderFor(server);
  u.forgetPending();
  const [r] = await u.uploadBatch([f], { title: 't' });
  assert.equal(r.state, 'done');
  assert.equal(server.finalBytes['a.mp3'].length, 300);
  void rec;
});

test('a corrupted chunk (bad CRC at the server) is retried', async () => {
  const server = new FakeServer();
  server.hooks.status = { 1: 400 }; // first chunk answered "400 bad checksum" after landing is not modelled; use CRC path below
  const [r] = await uploaderFor(server).uploadBatch([file('a.mp3', 250)], { title: 't' });
  assert.equal(r.state, 'done');
});

test('retries are bounded and the batch stays resumable', async () => {
  const server = new FakeServer();
  server.hooks.beforeChunk = () => {
    throw new UploadError('network down');
  };
  const u = uploaderFor(server);
  await assert.rejects(u.uploadBatch([file('a.mp3', 250)], { title: 't' }), /network down/);
  const chunkCalls = server.calls.filter((c) => c === 'CHUNK').length;
  assert.equal(chunkCalls, MAX_CHUNK_RETRIES + 1);
  assert.ok(u.pending(), 'the pending record must survive so that picking the same files resumes');
});

test('picking the same files again resumes in the same folder without a second start', async () => {
  const server = new FakeServer();
  server.hooks.beforeChunk = (n) => {
    if (n >= 3) throw new UploadError('network down');
  };
  const f = file('a.mp3', 500);
  const u = uploaderFor(server);
  await assert.rejects(u.uploadBatch([f], { title: 'Album' }));
  assert.equal(server.starts, 1);
  const uploadedSoFar = server.sessions.get('uploads/Album|a.mp3').bytes.length;
  assert.ok(uploadedSoFar > 0 && uploadedSoFar < 500);

  server.hooks.beforeChunk = null;
  server.chunkNo = 0;
  server.calls.length = 0;
  const [r] = await u.uploadBatch([f], { title: 'Album' }); // "after a reload"
  assert.equal(r.state, 'done');
  assert.equal(server.starts, 1, 'the folder is reused');
  const resent = 500 - uploadedSoFar;
  assert.equal(server.calls.filter((c) => c === 'CHUNK').length, Math.ceil(resent / 100), 'only the missing bytes were sent');
  assert.equal(u.pending(), null, 'finished batches are forgotten');
});

test('a different set of files gets its own folder', async () => {
  const server = new FakeServer();
  const u = uploaderFor(server);
  server.hooks.beforeChunk = () => {
    throw new UploadError('down');
  };
  await assert.rejects(u.uploadBatch([file('a.mp3', 150)], { title: 'One' }));
  server.hooks.beforeChunk = null;
  await u.uploadBatch([file('b.mp3', 150)], { title: 'One' });
  assert.equal(server.starts, 2);
});

test('an archive is polled until the server finishes unpacking', async () => {
  const server = new FakeServer();
  const messages = [];
  const progress = [];
  const [r] = await uploaderFor(server).uploadBatch([file('album.zip', 250)], {
    title: 'Album',
    onStatus: (m) => messages.push(m),
    onProgress: (p) => progress.push(p),
  });
  assert.equal(r.state, 'done');
  assert.equal(r.tracks, 12);
  assert.equal(r.skipped, 3);
  assert.ok(messages.some((m) => m.startsWith('Unpacking album.zip')));
  assert.ok(progress.some((p) => p.extracting !== undefined));
  assert.equal(server.calls.filter((c) => c === 'GET /api/upload/status').length, 3);
});

test('a failed archive is reported, not thrown, and the next file still uploads', async () => {
  const server = new FakeServer();
  server.hooks.archiveFails = 'contains no audio files';
  const results = await uploaderFor(server).uploadBatch([file('junk.zip', 120), file('good.mp3', 120)], { title: 't' });
  assert.equal(results[0].state, 'failed');
  assert.match(results[0].error, /no audio/);
  assert.equal(results[1].state, 'done');
});

test('a corruption verdict makes the client resend from the rewind point, once', async () => {
  const server = new FakeServer();
  server.hooks.corruptOnce = 100;
  const payload = Uint8Array.from({ length: 300 }, (_, i) => (i * 3) % 256);
  const messages = [];
  const [r] = await uploaderFor(server).uploadBatch([new File([payload], 'a.mp3')], { title: 't', onStatus: (m) => messages.push(m) });
  assert.equal(r.state, 'done');
  assert.deepEqual([...server.finalBytes['a.mp3']], [...payload]);
  assert.ok(messages.some((m) => m.includes('integrity check')));
});

test('a second corruption verdict is final, not an endless loop', async () => {
  const server = new FakeServer();
  let completes = 0;
  const original = server.json.bind(server);
  server.json = async (method, url, body) => {
    if (url.startsWith('/api/upload/complete')) {
      completes++;
      return { state: 'corrupted', error: 'still damaged', resumeOffset: 0, totalBytes: 100, bytesWritten: 0 };
    }
    return original(method, url, body);
  };
  const [r] = await uploaderFor(server).uploadBatch([file('a.mp3', 100)], { title: 't' });
  assert.equal(r.state, 'corrupted');
  assert.equal(completes, 2);
});

test('the server forgetting a running extraction (restart) is recovered by asking again', async () => {
  const server = new FakeServer();
  server.hooks.lose = true;
  const [r] = await uploaderFor(server).uploadBatch([file('album.zip', 100)], { title: 't' });
  assert.equal(r.state, 'done');
  assert.ok(server.calls.filter((c) => c === 'POST /api/upload/complete').length >= 2);
});

test('losing track of the extraction over and over eventually gives up on that file', async () => {
  const server = new FakeServer();
  const original = server.json.bind(server);
  server.json = async (method, url, body) => {
    if (url.startsWith('/api/upload/status')) throw new UploadError('nothing tracked', 404);
    return original(method, url, body);
  };
  const [r] = await uploaderFor(server).uploadBatch([file('album.zip', 100)], { title: 't' });
  assert.equal(r.state, 'failed');
  assert.match(r.error, /nothing tracked/);
  // It asked the server to re-complete a few times before giving up (not forever).
  assert.ok(server.calls.filter((c) => c === 'POST /api/upload/complete').length <= 5);
});

test('per-file refusals are recorded and the batch continues; auth problems stop it', async () => {
  const server = new FakeServer();
  server.hooks.refuse = (body) => {
    if (body.filename === 'big.mp3') throw new UploadError('upload too large', 413);
    if (body.filename === 'full.mp3') throw new UploadError('not enough free space', 507);
  };
  const results = await uploaderFor(server).uploadBatch([file('big.mp3', 100), file('full.mp3', 100), file('ok.mp3', 100)], { title: 't' });
  assert.deepEqual(results.map((r) => r.state), ['failed', 'failed', 'done']);
  assert.match(results[0].error, /too large/);
  assert.match(results[1].error, /free space/);

  const s2 = new FakeServer();
  s2.hooks.refuse = () => {
    throw new UploadError('login required', 401);
  };
  const u2 = uploaderFor(s2);
  await assert.rejects(u2.uploadBatch([file('a.mp3', 100)], { title: 't' }), /login required/);
  assert.ok(u2.pending(), 'signed out mid-batch: keep the record for later');

  const s3 = new FakeServer();
  s3.hooks.refuse = () => {
    throw new UploadError('server error', 500);
  };
  await assert.rejects(uploaderFor(s3).uploadBatch([file('a.mp3', 100)], { title: 't' }), /server error/);
});

test('being signed out or forbidden mid-chunk is not retried', async () => {
  for (const status of [401, 403]) {
    const server = new FakeServer();
    server.hooks.status = { 1: status };
    await assert.rejects(uploaderFor(server).uploadBatch([file('a.mp3', 250)], { title: 't' }));
    assert.equal(server.calls.filter((c) => c === 'CHUNK').length, 1);
  }
});

test('storage that throws is tolerated', async () => {
  const server = new FakeServer();
  const broken = {
    getItem() {
      throw new Error('denied');
    },
    setItem() {
      throw new Error('denied');
    },
    removeItem() {
      throw new Error('denied');
    },
  };
  const u = createUploader({ transport: server, sleep: noSleep, chunkSize: 100, storage: broken });
  const [r] = await u.uploadBatch([file('a.mp3', 150)], { title: 't' });
  assert.equal(r.state, 'done');
  assert.equal(u.pending(), null);
  u.forgetPending();
  const none = createUploader({ transport: server, sleep: noSleep, storage: null });
  assert.equal(none.pending(), null);
});

test('batch signature ignores order', () => {
  assert.equal(batchSignature([file('b', 2), file('a', 1)]), batchSignature([file('a', 1), file('b', 2)]));
  assert.notEqual(batchSignature([file('a', 1)]), batchSignature([file('a', 2)]));
});
