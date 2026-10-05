import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { rankImages, isImageBytes, pick, collect, MAX_IMAGES_PER_FOLDER } from '../../scripts/delta-for-upload.mjs';

test('covers are ranked like the server does', () => {
  assert.deepEqual(
    rankImages(['notes.txt', '03.jpg', 'front-scan.png', 'Cover.JPG', 'a.gif', 'folder.png', 'B.webp', 'song.mp3', 'Album.jpg']),
    ['Cover.JPG', 'folder.png', 'Album.jpg', 'front-scan.png', '03.jpg', 'a.gif', 'B.webp'],
  );
});

test('pictures are recognised by content', () => {
  const b = (...x) => Buffer.from(x);
  assert.ok(isImageBytes(b(0xff, 0xd8, 0xff, 0xe0)));
  assert.ok(isImageBytes(Buffer.from('\x89PNG\r\n\x1a\n rest', 'latin1')));
  assert.ok(isImageBytes(Buffer.from('GIF89a..')));
  assert.ok(isImageBytes(Buffer.from('RIFF\0\0\0\0WEBPVP8 ')));
  assert.ok(!isImageBytes(Buffer.from('\x89PNG\r\n', 'latin1')), 'a truncated PNG signature is not a picture');
  assert.ok(!isImageBytes(Buffer.from('<svg onload=alert(1)>')));
  assert.ok(!isImageBytes(Buffer.alloc(0)));
});

test('a folder listing yields pictures, ffmpeg audio and archives only', () => {
  const r = pick([
    { name: 'a.mp3', size: 5 }, { name: 'b.wma', size: 5 }, { name: 'C.APE', size: 5 }, { name: 'cover.jpg', size: 5 },
    { name: 'huge.jpg', size: 17 << 20 }, { name: 'more.7z', size: 5 }, { name: 'x.txt', size: 5 },
  ]);
  assert.deepEqual(r, { images: ['cover.jpg'], audio: ['b.wma', 'C.APE'], archives: ['more.7z'] });
});

test('inside a nested archive everything but pictures beyond the limit is new', () => {
  const r = pick([{ name: 'a.mp3', size: 1 }, { name: 'b.flac', size: 1 }, { name: 'cover.jpg', size: 1 }, { name: 'log.txt', size: 1 }, { name: 'deeper.zip', size: 1 }], true);
  assert.deepEqual(r, { images: ['cover.jpg'], audio: ['a.mp3', 'b.flac', 'log.txt'], archives: ['deeper.zip'] });
});

test('collect takes at most three real pictures per folder and the ffmpeg audio, nothing else', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'delta-test-'));
  try {
    const put = (rel, data) => {
      fs.mkdirSync(path.dirname(path.join(root, rel)), { recursive: true });
      fs.writeFileSync(path.join(root, rel), data);
    };
    const jpg = Buffer.from([0xff, 0xd8, 0xff, 0xe0, 1, 2]);
    put('Album/01.mp3', 'x');
    put('Album/song.wma', 'x');
    for (const n of ['cover.jpg', 'back.jpg', 'a.jpg', 'b.jpg', 'c.jpg']) put(`Album/${n}`, jpg);
    put('Album/fake.jpg', 'not a picture');
    put('Album/Scans/page.png', Buffer.from('\x89PNG\r\n\x1a\n.', 'latin1'));
    put('Album/.hidden.jpg', jpg);
    put('top.7z', 'archive in the top folder');
    put('Album/inner.7z', 'x');
    const taken = [];
    const unpacked = [];
    const ctx = {
      depth: 0, skippedTop: [],
      take: (from, rel, kind) => taken.push(`${kind}:${rel}`),
      unpack: (from, rel) => unpacked.push(rel),
    };
    collect(root, '', ctx);
    assert.deepEqual(taken.sort(), ['audio:Album/song.wma', 'image:Album/Scans/page.png', 'image:Album/a.jpg', 'image:Album/b.jpg', 'image:Album/cover.jpg'].sort());
    assert.equal(MAX_IMAGES_PER_FOLDER, 3);
    assert.deepEqual(unpacked, ['Album/inner.7z']);
    assert.deepEqual(ctx.skippedTop, ['top.7z']);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
