// Chunked, resumable upload client. Port of anime-db-tracker's useLibraryUpload
// (same protocol as internal/upload on the server):
//
//   start    -> {relPath}              one folder per batch
//   begin    -> {offset}               open or resume a file; the SERVER says how many bytes it has
//   chunk    -> {offset}               16 MiB at a time, with a CRC32 the server verifies
//   complete -> {state, ...}           archives extract in the background ("running")
//   status   -> {state, ...}           polled while running
//
// Any failure (bad checksum, dropped connection, a reload) is handled the same
// way: wait, ask the server for the real offset, and continue from there.
// One deliberate difference from the original: after a failed chunk the next
// chunk is cut again from the offset the server reports, rather than re-sending
// the old bytes at a new offset (which could duplicate data if the failed
// request had actually landed).

export const CHUNK_SIZE = 16 * 1024 * 1024;
export const MAX_CHUNK_RETRIES = 8;
export const MAX_COMPLETE_ATTEMPTS = 2; // one retry to recover from a localised corrupt chunk
export const STATUS_POLL_MS = 3000;
const MAX_LOST_STATUS = 3;
const PENDING_KEY = 'mmp-upload-batch';

// ------------------------------------------------------------------ CRC32

let crcTable = null;
function table() {
  if (crcTable) return crcTable;
  crcTable = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    crcTable[n] = c >>> 0;
  }
  return crcTable;
}

// Plain-JS CRC32 (IEEE), deliberately not crypto.subtle: that only exists in a
// secure context (HTTPS or localhost), and this app is often used over plain HTTP.
export function crc32Bytes(bytes) {
  const t = table();
  let crc = 0xffffffff;
  for (let i = 0; i < bytes.length; i++) crc = t[(crc ^ bytes[i]) & 0xff] ^ (crc >>> 8);
  return ((crc ^ 0xffffffff) >>> 0).toString(16).padStart(8, '0');
}

export async function hashChunk(blob) {
  return crc32Bytes(new Uint8Array(await blob.arrayBuffer()));
}

// ------------------------------------------------------------------ transport

export class UploadError extends Error {
  constructor(message, status = 0) {
    super(message);
    this.status = status; // 0 = network failure
  }
}

const sleepFor = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// The real transport: fetch for JSON, XHR for chunks (it reports upload progress).
export function httpTransport() {
  const H = { 'X-Requested-With': 'masterplayer' };
  return {
    async json(method, url, body) {
      let res;
      try {
        res = await fetch(url, {
          method,
          credentials: 'same-origin',
          headers: body === undefined ? H : { ...H, 'Content-Type': 'application/json' },
          body: body === undefined ? undefined : JSON.stringify(body),
        });
      } catch (err) {
        throw new UploadError(err.message || 'network error');
      }
      let data = null;
      try {
        data = await res.json();
      } catch {
        /* no body */
      }
      if (!res.ok) throw new UploadError((data && data.error) || res.statusText, res.status);
      return data;
    },
    chunk(url, headers, blob, onProgress) {
      return new Promise((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        xhr.open('POST', url);
        xhr.setRequestHeader('X-Requested-With', 'masterplayer');
        for (const [k, v] of Object.entries(headers)) xhr.setRequestHeader(k, v);
        xhr.upload.onprogress = (e) => e.lengthComputable && onProgress?.(e.loaded);
        xhr.onerror = () => reject(new UploadError('network error'));
        xhr.ontimeout = () => reject(new UploadError('timed out'));
        xhr.onload = () => {
          let body = null;
          try {
            body = JSON.parse(xhr.responseText);
          } catch {
            /* no body */
          }
          resolve({ status: xhr.status, body });
        };
        xhr.send(blob);
      });
    },
  };
}

// ------------------------------------------------------------------ pending batch

function safeStorage() {
  try {
    return globalThis.localStorage || null;
  } catch {
    return null;
  }
}

// A definite "no" for one file (400, 404, 413, 415, 507...), as opposed to a failure that
// is worth resuming (no connection, signed out, timeouts, server errors).
function isVerdict(status) {
  if (status === 507) return true;
  return status >= 400 && status < 500 && ![401, 403, 408, 429].includes(status);
}

export function batchSignature(files) {
  return files
    .map((f) => `${f.name}:${f.size}`)
    .sort()
    .join('|');
}

// ------------------------------------------------------------------ uploader

export function createUploader({
  transport = httpTransport(),
  sleep = sleepFor,
  chunkSize = CHUNK_SIZE,
  hash = hashChunk,
  retryBaseMs = 1000,
  pollMs = STATUS_POLL_MS,
  storage = safeStorage(),
} = {}) {
  const q = (params) => new URLSearchParams(params).toString();

  const store = {
    load() {
      try {
        return JSON.parse(storage?.getItem(PENDING_KEY) || 'null');
      } catch {
        return null;
      }
    },
    save(rec) {
      try {
        storage?.setItem(PENDING_KEY, JSON.stringify(rec));
      } catch {
        /* best effort: worst case a reload starts a new folder */
      }
    },
    clear() {
      try {
        storage?.removeItem(PENDING_KEY);
      } catch {
        /* nothing to clean */
      }
    },
  };

  const begin = async (relPath, file) =>
    (await transport.json('POST', '/api/upload/begin', { relPath, filename: file.name, size: file.size })).offset;

  // Sends everything from `offset` to the end of the file.
  async function sendChunks(relPath, file, offset, report, status) {
    let failures = 0;
    while (offset < file.size) {
      const blob = file.slice(offset, Math.min(offset + chunkSize, file.size));
      const at = offset;
      try {
        const crc = await hash(blob);
        const url = `/api/upload/chunk?${q({ relPath, filename: file.name, offset: at })}`;
        const res = await transport.chunk(url, { 'X-Chunk-CRC32': crc }, blob, (loaded) => report(at + loaded));
        if (res.status === 200) {
          offset = res.body.offset;
          failures = 0;
          report(offset);
          continue;
        }
        if (res.status === 409) {
          offset = res.body.offset; // the server's idea of "how far" wins; just continue from it
          continue;
        }
        throw new UploadError((res.body && res.body.error) || `chunk rejected (HTTP ${res.status})`, res.status);
      } catch (err) {
        if (err.status === 401 || err.status === 403) throw err; // retrying cannot help
        failures++;
        if (failures > MAX_CHUNK_RETRIES) throw err;
        status(`${file.name}: retrying after a dropped chunk (${failures}/${MAX_CHUNK_RETRIES})…`);
        await sleep(Math.min(retryBaseMs * 2 ** (failures - 1), 30000));
        offset = await begin(relPath, file); // the true offset; the next chunk is cut from here
      }
    }
    return offset;
  }

  // Polls while an archive is verified and extracted on the server.
  async function pollUntilTerminal(relPath, file, report, status) {
    let lost = 0;
    for (;;) {
      let st;
      try {
        st = await transport.json('GET', `/api/upload/status?${q({ relPath, filename: file.name })}`);
        lost = 0;
      } catch (err) {
        // 404: the server forgot (it restarted). complete is idempotent, so ask again.
        if (err.status !== 404 || ++lost > MAX_LOST_STATUS) throw err;
        st = await transport.json('POST', '/api/upload/complete', { relPath, filename: file.name, size: file.size });
      }
      if (st.state !== 'running') return st;
      const pct = st.totalBytes > 0 ? Math.round((st.bytesWritten / st.totalBytes) * 100) : 0;
      report(file.size, { extracting: st.bytesWritten / Math.max(st.totalBytes, 1) });
      status(`Unpacking ${file.name} on the server… ${pct}%`);
      await sleep(pollMs);
    }
  }

  async function uploadOne(relPath, file, report, status) {
    let offset = await begin(relPath, file);
    offset = await sendChunks(relPath, file, offset, report, status);
    for (let attempt = 1; ; attempt++) {
      status(`Finishing ${file.name}…`);
      let st = await transport.json('POST', '/api/upload/complete', { relPath, filename: file.name, size: file.size });
      if (st.state === 'running') st = await pollUntilTerminal(relPath, file, report, status);
      if (st.state === 'corrupted' && attempt < MAX_COMPLETE_ATTEMPTS) {
        // The server localised damage to one chunk and rewound the session: resend from there.
        status(`${file.name} failed its integrity check; recovering…`);
        offset = await begin(relPath, file);
        offset = await sendChunks(relPath, file, offset, report, status);
        continue;
      }
      return st;
    }
  }

  return {
    // The interrupted batch, if any (a reload cannot reopen files, so the user re-selects them).
    pending: () => store.load(),
    forgetPending: () => store.clear(),

    // Uploads files (in order) into one folder named `title` ("" = the upload folder itself).
    // Resolves with one result per file; rejects only when the connection is truly gone, leaving
    // the pending record so that choosing the same files again resumes.
    async uploadBatch(files, { title = '', onProgress = () => {}, onStatus = () => {} } = {}) {
      const total = files.reduce((n, f) => n + f.size, 0) || 1;
      const sig = batchSignature(files);
      let rec = store.load();
      if (!rec || rec.sig !== sig || rec.title !== title) {
        onStatus('Starting…');
        const { relPath } = await transport.json('POST', '/api/upload/start', { title });
        rec = { sig, title, relPath, names: files.map((f) => f.name) };
        store.save(rec); // a reload mid-batch reuses this folder instead of minting another
      }

      const results = [];
      let before = 0;
      for (const file of files) {
        const report = (fileBytes, extra = {}) =>
          onProgress({ fraction: Math.min(1, (before + fileBytes) / total), file: file.name, ...extra });
        try {
          const st = await uploadOne(rec.relPath, file, report, onStatus);
          results.push({ name: file.name, size: file.size, ...st });
        } catch (err) {
          // The server refused this particular file (too large, no space, wrong type...): note it and go on.
          // Anything else (no connection, signed out, a server hiccup) ends the batch so it can be resumed.
          if (!isVerdict(err.status)) throw err;
          results.push({ name: file.name, size: file.size, state: 'failed', error: err.message });
        }
        before += file.size;
        report(0);
      }
      store.clear();
      return results;
    },
  };
}
