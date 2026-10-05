import test from 'node:test';
import assert from 'node:assert/strict';
import { suggestTitle, describeResult, summarize, isArchive, is7z } from '../../web/js/uploadview.js';
import { fmtBytes } from '../../web/js/util.js';

const f = (name) => ({ name, size: 10 });

test('archives are recognised by extension, case-insensitively', () => {
  assert.ok(isArchive('a.zip') && isArchive('B.ZIP') && isArchive('c.7z'));
  assert.ok(!isArchive('d.mp3') && !isArchive('zip') && !isArchive('e.rar'));
  assert.ok(is7z('x.7Z') && !is7z('x.zip'));
});

test('a typed folder name always wins', () => {
  assert.equal(suggestTitle([f('album.zip')], '  My Mix '), 'My Mix');
  assert.equal(suggestTitle([f('a.mp3'), f('b.mp3')], 'Mix'), 'Mix');
});

test('one lone archive gets a folder named after it; loose files go to the upload folder', () => {
  assert.equal(suggestTitle([f('Great Album.zip')]), 'Great Album');
  assert.equal(suggestTitle([f('pack.v2.7z')]), 'pack.v2');
  assert.equal(suggestTitle([f('a.mp3')]), '');
  assert.equal(suggestTitle([f('a.mp3'), f('b.mp3')]), '');
  assert.equal(suggestTitle([f('a.zip'), f('b.zip')]), '');
  assert.equal(suggestTitle([]), '');
});

test('results read naturally', () => {
  assert.equal(describeResult({ state: 'done', tracks: 1 }), 'Added to the library');
  assert.equal(describeResult({ state: 'done', tracks: 12 }), '12 tracks added');
  assert.equal(describeResult({ state: 'done', tracks: 12, skipped: 3 }), '12 tracks added, 3 other files skipped');
  assert.equal(describeResult({ state: 'done', tracks: 1, skipped: 1 }), '1 track added, 1 other file skipped');
  assert.equal(describeResult({ state: 'failed', error: 'not an audio file' }), 'not an audio file');
  assert.equal(describeResult({ state: 'failed' }), 'Upload failed');
});

test('batch summaries', () => {
  assert.deepEqual(summarize([{ state: 'done', tracks: 1 }]), { tracks: 1, bad: 0, text: 'Added 1 track' });
  assert.deepEqual(summarize([{ state: 'done', tracks: 5 }, { state: 'done', tracks: 7 }]), { tracks: 12, bad: 0, text: 'Added 12 tracks' });
  assert.deepEqual(summarize([{ state: 'done', tracks: 2 }, { state: 'failed' }]), { tracks: 2, bad: 1, text: 'Added 2 tracks; 1 failed' });
  assert.equal(summarize([{ state: 'failed' }]).text, 'Nothing was added (1 file failed)');
  assert.equal(summarize([{ state: 'failed' }, { state: 'corrupted' }]).text, 'Nothing was added (2 files failed)');
});

test('byte sizes', () => {
  assert.equal(fmtBytes(12), '12 B');
  assert.equal(fmtBytes(2048), '2 KB');
  assert.equal(fmtBytes(5 * 1024 ** 2), '5.0 MB');
  assert.equal(fmtBytes(3 * 1024 ** 3), '3.00 GB');
  assert.equal(fmtBytes(-1), '');
  assert.equal(fmtBytes(NaN), '');
});
