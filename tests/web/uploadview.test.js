import test from 'node:test';
import assert from 'node:assert/strict';
import { suggestTitle, describeResult, describeTypes, reportUrl, summarize, isArchive, is7z } from '../../web/js/uploadview.js';
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
  assert.equal(
    describeResult({ state: 'done', tracks: 9136, skipped: 1173, skippedTypes: { txt: 66, jpg: 927, png: 100, log: 20 } }),
    '9136 tracks added, 1173 other files skipped (jpg ×927, png ×100, txt ×66, log ×20)',
  );
  assert.equal(describeResult({ state: 'done', tracks: 3, duplicates: 59 }), '3 tracks added, 59 already in the library');
  assert.equal(describeResult({ state: 'done', tracks: 0, duplicates: 1 }), 'Nothing new: 1 already in the library');
  assert.equal(describeResult({ state: 'done', tracks: 0, duplicates: 4, skipped: 2 }), 'Nothing new: 4 already in the library, 2 other files skipped');
  assert.equal(
    describeResult({ state: 'done', tracks: 2, duplicates: 5, skipped: 1, skippedTypes: { txt: 1 } }),
    '2 tracks added, 5 already in the library, 1 other file skipped (txt ×1)',
  );
  assert.equal(describeResult({ state: 'failed', error: 'not an audio file' }), 'not an audio file');
  assert.equal(describeResult({ state: 'failed' }), 'Upload failed');
});

test('skipped types are grouped, biggest first, with a tail', () => {
  assert.equal(describeTypes({}), '');
  assert.equal(describeTypes(undefined), '');
  assert.equal(describeTypes({ b: 2, a: 2, c: 5 }), 'c ×5, a ×2, b ×2');
  const many = { a: 9, b: 8, c: 7, d: 6, e: 5, f: 4, g: 3, h: 2 };
  assert.equal(describeTypes(many), 'a ×9, b ×8, c ×7, d ×6, e ×5, f ×4, 2 more types');
  assert.equal(describeTypes({ ...many, i: 1 }, 8), 'a ×9, b ×8, c ×7, d ×6, e ×5, f ×4, g ×3, h ×2, 1 more type');
});

test('the report link needs a saved report', () => {
  assert.equal(reportUrl({ name: 'a.zip', path: 'x' }), '');
  assert.equal(reportUrl({ name: 'my music.7z', path: 'My Album', hasReport: true }), '/api/upload/report?relPath=My%20Album&filename=my%20music.7z');
  assert.equal(reportUrl({ name: 'a.zip', hasReport: true }), '/api/upload/report?relPath=&filename=a.zip');
});

test('batch summaries', () => {
  assert.deepEqual(summarize([{ state: 'done', tracks: 1 }]), { tracks: 1, bad: 0, text: 'Added 1 track' });
  assert.deepEqual(summarize([{ state: 'done', tracks: 5 }, { state: 'done', tracks: 7 }]), { tracks: 12, bad: 0, text: 'Added 12 tracks' });
  assert.deepEqual(summarize([{ state: 'done', tracks: 2 }, { state: 'failed' }]), { tracks: 2, bad: 1, text: 'Added 2 tracks; 1 failed' });
  assert.equal(summarize([{ state: 'done', tracks: 4, duplicates: 2 }]).text, 'Added 4 tracks, 2 already there');
  assert.equal(summarize([{ state: 'done', tracks: 0, duplicates: 7 }]).text, 'Nothing new: 7 already in the library');
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

test('format labels use the extension for converted formats', async () => {
  const { kindLabel } = await import('../../web/js/util.js');
  assert.equal(kindLabel('ffmpeg', 'A/b.wma'), 'WMA');
  assert.equal(kindLabel('vgm', 'A/b.brstm'), 'BRSTM');
  assert.equal(kindLabel('ffmpeg', 'noext'), 'FFMPEG');
  assert.equal(kindLabel('mp3', 'A/b.mp3'), 'MP3');
  assert.equal(kindLabel('', 'x'), 'FILE');
  assert.equal(kindLabel('', 'x', ''), '');
});
