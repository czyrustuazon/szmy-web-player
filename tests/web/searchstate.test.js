import test from 'node:test';
import assert from 'node:assert/strict';
import { SearchState, SEP, stemOf, dirOf, searchText, highlight } from '../../web/js/searchstate.js';

const track = (path) => ({ path, name: path.split('/').pop() });
const library = [
  track('Final Fantasy VII/01 One-Winged Angel.flac'),
  track('Final Fantasy VII/02 Aerith.mp3'),
  track('Chrono Trigger/Corridors of Time.brstm'),
  track('loose song.mp3'),
];

test('names and folders are split for display', () => {
  assert.equal(stemOf('a.b.mp3'), 'a.b');
  assert.equal(stemOf('.hidden'), '.hidden');
  assert.equal(stemOf('noext'), 'noext');
  assert.equal(dirOf('A/B/c.mp3'), 'A / B');
  assert.equal(dirOf('c.mp3'), '');
  assert.equal(searchText(track('A/B/c.mp3')), `c${SEP}A / B`);
});

test('folder names are searchable, so artist and album queries find tracks', () => {
  const s = new SearchState();
  const hits = s.run(library, 'final fantasy');
  assert.deepEqual(s.items().map((t) => t.path), ['Final Fantasy VII/01 One-Winged Angel.flac', 'Final Fantasy VII/02 Aerith.mp3']);
  assert.equal(hits.length, 2);
  assert.ok(s.active);
});

test('typos and scattered letters find tracks too', () => {
  const s = new SearchState();
  s.run(library, 'aeirth');
  assert.equal(s.items()[0].name, '02 Aerith.mp3');
  s.run(library, 'crdrs tme');
  assert.equal(s.items()[0].name, 'Corridors of Time.brstm');
});

test('the index is reused for the same list and rebuilt for a different one', () => {
  const s = new SearchState();
  s.run(library, 'fantasy');
  const first = s.index;
  s.run(library, 'chrono');
  assert.equal(s.index, first, 'same pool, same index');
  s.run([...library], 'chrono');
  assert.notEqual(s.index, first, 'a new array (library changed) gets a new index');
});

test('an empty query clears the results', () => {
  const s = new SearchState();
  s.run(library, 'fantasy');
  assert.ok(s.hits.length);
  s.run(library, '   ');
  assert.equal(s.active, false);
  assert.deepEqual(s.hits, []);
  assert.deepEqual(s.items(), []);
});

test('hits can be removed (a deleted track disappears from the results)', () => {
  const s = new SearchState();
  s.run(library, 'fantasy');
  s.remove('Final Fantasy VII/02 Aerith.mp3');
  assert.deepEqual(s.items().map((t) => t.name), ['01 One-Winged Angel.flac']);
  assert.equal(s.marksFor('Final Fantasy VII/02 Aerith.mp3'), null);
  s.remove('not there'); // harmless
});

test('highlight marks in the name and the folder line line up', () => {
  const s = new SearchState();
  s.run([track('Final Fantasy VII/02 Aerith.mp3')], 'aerith');
  const [hit] = s.hits;
  const name = stemOf(hit.item.name);
  assert.equal(highlight(name, hit.marks, 0), '02 <mark>Aerith</mark>');
  assert.equal(highlight(dirOf(hit.item.path), hit.marks, name.length + SEP.length), 'Final Fantasy VII');

  s.run([track('Final Fantasy VII/02 Aerith.mp3')], 'fantasy');
  const m = s.hits[0];
  assert.equal(highlight(name, m.marks, 0), '02 Aerith');
  assert.equal(highlight(dirOf(m.item.path), m.marks, name.length + SEP.length), 'Final <mark>Fantasy</mark> VII');
});

test('highlighting escapes HTML in file names', () => {
  assert.equal(highlight('<b>&"', [1], 0), '&lt;<mark>b</mark>&gt;&amp;&quot;');
  assert.equal(highlight('abc', [], 0), 'abc');
  assert.equal(highlight('abc', [0, 1, 2], 0), '<mark>abc</mark>');
  assert.equal(highlight('abc', [0, 2], 0), '<mark>a</mark>b<mark>c</mark>');
});
