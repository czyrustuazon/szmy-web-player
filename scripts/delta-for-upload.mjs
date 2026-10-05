// Builds a small .zip of what a music folder has that the player may be missing, so that you can
// add it to an album that is already uploaded instead of sending everything again:
//
//   - cover pictures: up to three real images per folder, cover/folder/front... first (the same
//     rule the server applies), which the player uses as cover art;
//   - audio in formats the server could not play before ffmpeg (WMA, APE, WavPack, FLV, ...);
//   - archives found inside the folders (.7z .zip .rar) are unpacked here and the same rules apply
//     to what is in them, in a folder named after the archive.
//
// Everything else (the mp3/flac files that are already on the server) is left out.
//
//   node scripts/delta-for-upload.mjs "E:\Z_Gen Music\music" delta.zip
//
// Then, on the Upload tab: choose delta.zip, type the folder name the music was uploaded under
// (for example "music (2)"), tick "Add to the folder with this name if it already exists" and
// upload. Files already on the server are skipped, so running it twice does no harm.
//
// Needs 7-Zip (7z on PATH, or in C:\Program Files\7-Zip, or set SEVENZIP).

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { pathToFileURL } from 'node:url';

export const MAX_IMAGES_PER_FOLDER = 3;
export const MAX_IMAGE_BYTES = 16 << 20;
const IMAGE_EXTS = new Set(['.jpg', '.jpeg', '.png', '.webp', '.gif']);
const COVER_NAMES = ['cover', 'folder', 'front', 'album', 'albumart', 'art', 'artwork', 'thumb'];
// Keep in step with internal/sniff (ffmpegExts).
export const FFMPEG_EXTS = new Set(
  'wma wmv asf ape wv tta mka mpc dsf dff flv avi mkv mov webm 3gp amr ac3 dts aif aiff aifc mp2 caf'.split(' ').map((e) => '.' + e),
);
export const NESTED_ARCHIVES = new Set(['.7z', '.zip', '.rar']);

const ext = (name) => path.extname(name).toLowerCase();

// Mirrors library.RankImages: best cover candidate first.
export function rankImages(names) {
  const rank = (n) => {
    const base = path.basename(n, path.extname(n)).toLowerCase();
    const i = COVER_NAMES.indexOf(base);
    if (i >= 0) return i;
    return base.includes('cover') || base.includes('front') ? COVER_NAMES.length : COVER_NAMES.length + 1;
  };
  return names
    .filter((n) => IMAGE_EXTS.has(ext(n)))
    .sort((a, b) => rank(a) - rank(b) || (a.toLowerCase() < b.toLowerCase() ? -1 : a.toLowerCase() > b.toLowerCase() ? 1 : 0));
}

// A picture by content, as the server checks it (JPEG, PNG, GIF or WebP signature).
export function isImageBytes(b) {
  const at = (i, s) => b.length >= i + s.length && [...s].every((c, k) => b[i + k] === c.charCodeAt(0));
  return (
    (b.length >= 3 && b[0] === 0xff && b[1] === 0xd8 && b[2] === 0xff) ||
    (b.length >= 8 && b[0] === 0x89 && at(1, 'PNG') && b[4] === 0x0d && b[5] === 0x0a && b[6] === 0x1a && b[7] === 0x0a) ||
    at(0, 'GIF87a') ||
    at(0, 'GIF89a') ||
    (at(0, 'RIFF') && at(8, 'WEBP'))
  );
}

function readHead(file, n = 16) {
  const fd = fs.openSync(file, 'r');
  try {
    const buf = Buffer.alloc(n);
    return buf.subarray(0, fs.readSync(fd, buf, 0, n, 0));
  } finally {
    fs.closeSync(fd);
  }
}

// Decides what to take from one folder listing. `files` are {name, size}; returns the names to
// copy and, separately, the nested archives to open.
export function pick(files) {
  const images = rankImages(files.filter((f) => f.size <= MAX_IMAGE_BYTES).map((f) => f.name));
  return {
    images,
    audio: files.filter((f) => FFMPEG_EXTS.has(ext(f.name))).map((f) => f.name),
    archives: files.filter((f) => NESTED_ARCHIVES.has(ext(f.name))).map((f) => f.name),
  };
}

function find7z() {
  const candidates = [process.env.SEVENZIP, '7z', 'C:\\Program Files\\7-Zip\\7z.exe', 'C:\\Program Files (x86)\\7-Zip\\7z.exe'].filter(Boolean);
  for (const c of candidates) {
    const r = spawnSync(c, [], { encoding: 'utf8' });
    if (!r.error) return c;
  }
  throw new Error('7-Zip was not found. Install it, put 7z on PATH, or set SEVENZIP to its full path.');
}

export function collect(dir, rel, ctx) {
  const entries = fs.readdirSync(dir, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name));
  const files = [];
  for (const e of entries) {
    const full = path.join(dir, e.name);
    if (e.isSymbolicLink() || e.name.startsWith('.')) continue;
    if (e.isDirectory()) collect(full, rel ? `${rel}/${e.name}` : e.name, ctx);
    else if (e.isFile()) files.push({ name: e.name, size: fs.statSync(full).size });
  }
  const { images, audio, archives } = pick(files);
  const take = (name, kind) => {
    ctx.take(path.join(dir, name), rel ? `${rel}/${name}` : name, kind);
  };
  let kept = 0;
  for (const n of images) {
    if (kept >= MAX_IMAGES_PER_FOLDER) break;
    if (!isImageBytes(readHead(path.join(dir, n)))) continue; // a fake picture is dropped by the server too
    take(n, 'image');
    kept++;
  }
  audio.forEach((n) => take(n, 'audio'));
  // An archive sitting directly in the chosen folder is normally the one you already uploaded.
  if (rel !== '' || ctx.depth > 0) archives.forEach((n) => ctx.unpack(path.join(dir, n), rel ? `${rel}/${n}` : n));
  else archives.forEach((n) => ctx.skippedTop.push(n));
}

function main(argv) {
  const [src, out] = argv;
  if (!src || !out) {
    console.error('usage: node scripts/delta-for-upload.mjs <music folder> <output.zip>');
    process.exit(2);
  }
  if (!fs.statSync(src, { throwIfNoEntry: false })?.isDirectory()) throw new Error(`${src} is not a folder`);
  const sevenZ = find7z();
  const stage = fs.mkdtempSync(path.join(os.tmpdir(), 'mp-delta-'));
  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'mp-delta-unpack-'));
  const counts = { image: 0, audio: 0, archive: 0 };
  let bytes = 0;
  let unpacked = 0;
  const ctx = {
    depth: 0,
    skippedTop: [],
    take(from, rel, kind) {
      const to = path.join(stage, ...rel.split('/'));
      fs.mkdirSync(path.dirname(to), { recursive: true });
      fs.copyFileSync(from, to);
      counts[kind]++;
      bytes += fs.statSync(to).size;
    },
    unpack(archive, rel) {
      if (ctx.depth >= 3) return;
      const dest = path.join(scratch, String(++unpacked));
      console.log(`unpacking ${rel} ...`);
      const r = spawnSync(sevenZ, ['x', '-y', '-bd', `-o${dest}`, archive], { stdio: 'inherit' });
      if (r.status !== 0) {
        console.warn(`  could not unpack ${rel}; skipped`);
        return;
      }
      counts.archive++;
      ctx.depth++;
      collect(dest, rel.replace(/\.[^./]+$/, ''), ctx);
      ctx.depth--;
    },
  };
  try {
    collect(path.resolve(src), '', ctx);
    ctx.skippedTop.forEach((n) => console.log(`left out ${n} (an archive in the top folder; assumed to be what you already uploaded)`));
    console.log(`found ${counts.image} cover pictures, ${counts.audio} files in ffmpeg formats, ${counts.archive} nested archives opened (${(bytes / 1048576).toFixed(1)} MB)`);
    if (counts.image + counts.audio === 0) {
      console.log('nothing to add.');
      return;
    }
    fs.rmSync(out, { force: true });
    const r = spawnSync(sevenZ, ['a', '-tzip', '-mx=0', '-bd', path.resolve(out), '*'], { cwd: stage, stdio: 'inherit' });
    if (r.status !== 0) throw new Error('7-Zip could not create the zip');
    console.log(`\nwrote ${out} (${(fs.statSync(out).size / 1048576).toFixed(1)} MB)`);
    console.log('Upload it, name the folder as before and tick "Add to the folder with this name if it already exists".');
  } finally {
    fs.rmSync(stage, { recursive: true, force: true });
    fs.rmSync(scratch, { recursive: true, force: true });
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    main(process.argv.slice(2));
  } catch (err) {
    console.error(err.message);
    process.exit(1);
  }
}
