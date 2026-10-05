import test from 'node:test';
import assert from 'node:assert/strict';
import { normalize, editDistance, tokenize, fuzzyMatch, createIndex, searchIndex, search } from '../../web/js/fuzzy.js';

const names = (hits) => hits.map((h) => h.item);
const find = (items, q, limit) => names(search(items, q, (x) => x, limit));

test('normalisation ignores case and accents but never changes the length', () => {
  assert.equal(normalize('Café DÉJÀ Vu'), 'cafe deja vu');
  assert.equal(normalize('Ünïcödé'), 'unicode');
  for (const s of ['Café', '日本語のアルバム', 'ＡＢＣ', '😀 emoji', 'İstanbul', 'straße', '']) {
    assert.equal(normalize(s).length, s.length, s);
  }
  assert.equal(normalize('日本語'), '日本語');
});

test('edit distance counts substitutions, insertions, deletions and swaps as one', () => {
  assert.equal(editDistance('final', 'final', 2), 0);
  assert.equal(editDistance('final', 'fnial', 2), 1); // swapped letters
  assert.equal(editDistance('final', 'finl', 2), 1); // missing letter
  assert.equal(editDistance('final', 'finaal', 2), 1); // extra letter
  assert.equal(editDistance('final', 'fanal', 2), 1); // wrong letter
  assert.equal(editDistance('final', 'xxxxx', 2), 3); // gives up just past the limit
  assert.equal(editDistance('a', 'abcdef', 2), 3); // length gap alone is too much
});

test('a query is split into words, blanks ignored, capped at eight', () => {
  assert.deepEqual(tokenize('  Final   FANTASY '), ['final', 'fantasy']);
  assert.deepEqual(tokenize(''), []);
  assert.equal(tokenize('a b c d e f g h i j').length, 8);
});

test('substrings match, ignoring case and accents', () => {
  const items = ['Final Fantasy VII - Aerith', 'Chrono Trigger', 'Café del Mar'];
  assert.deepEqual(find(items, 'fantasy'), ['Final Fantasy VII - Aerith']);
  assert.deepEqual(find(items, 'CAFE'), ['Café del Mar']);
  assert.deepEqual(find(items, 'trig'), ['Chrono Trigger']);
});

test('every word of the query must match, in any order', () => {
  const items = ['Zelda - Song of Storms', 'Zelda - Lost Woods', 'Mario - Song of Storms'];
  assert.deepEqual(find(items, 'zelda storms'), ['Zelda - Song of Storms']);
  assert.deepEqual(find(items, 'storms zelda'), ['Zelda - Song of Storms']);
  assert.deepEqual(find(items, 'zelda mario'), []);
});

test('letters in order match even when spread out (fuzzy subsequence)', () => {
  const items = ['Final Fantasy VII', 'Chrono Trigger', 'Fantastic Four'];
  assert.deepEqual(find(items, 'fnlfntsy'), ['Final Fantasy VII']);
  assert.deepEqual(find(items, 'crnotrg'), ['Chrono Trigger']);
});

test('very short words only match as substrings, so they do not match everything', () => {
  assert.deepEqual(find(['Alpha', 'Beta', 'Gamma'], 'ga'), ['Gamma']);
  assert.deepEqual(find(['abcdef', 'a-b-c'], 'ac'), []); // "a...c" would match both as a subsequence
});

test('scattered letters across a long text are rejected', () => {
  assert.deepEqual(find(['a' + 'x'.repeat(60) + 'b' + 'x'.repeat(60) + 'c'], 'abc'), []);
});

test('typos are forgiven: swapped, missing, extra and wrong letters', () => {
  const items = ['Final Fantasy', 'Chrono Trigger', 'Metal Gear Solid'];
  assert.deepEqual(find(items, 'fnial'), ['Final Fantasy']); // swap
  assert.deepEqual(find(items, 'chrno'), ['Chrono Trigger']); // missing (also a subsequence)
  assert.deepEqual(find(items, 'triger'), ['Chrono Trigger']); // missing letter
  assert.deepEqual(find(items, 'fantacy'), ['Final Fantasy']); // wrong letter
  assert.deepEqual(find(items, 'metall'), ['Metal Gear Solid']); // extra letter
});

test('a half-typed word with a typo still matches the start of the word', () => {
  // 'fnatas' is 'fantas' (the start of 'fantasy') with two letters swapped.
  assert.deepEqual(find(['Final Fantasy VII'], 'fnatas'), ['Final Fantasy VII']);
  assert.deepEqual(find(['Final Fantasy VII'], 'fnial fantas'), ['Final Fantasy VII']);
  // two slips in a seven-letter word is one too many
  assert.deepEqual(find(['Final Fantasy VII'], 'fanatas'), []);
});

test('short words get no typo tolerance and long words get two', () => {
  assert.deepEqual(find(['Cat', 'Car'], 'cot'), []);
  assert.deepEqual(find(['Symphonic Metal'], 'symphnio'), ['Symphonic Metal']); // 8 letters: two edits from 'symphoni'
  assert.deepEqual(find(['Symphonic Metal'], 'symphonik'), ['Symphonic Metal']); // 9 letters: one wrong
  assert.deepEqual(find(['Symphonic Metal'], 'symfonik'), []); // three edits is too many
});

test('better matches rank first: exact, at the start, at a word boundary, whole word', () => {
  const items = ['A long preface about fantasy', 'Fantasy', 'Final Fantasy', 'Unfantastic', 'xfantasyx'];
  const hits = find(items, 'fantasy');
  assert.equal(hits[0], 'Fantasy'); // starts the text and ends the word
  assert.ok(hits.indexOf('Final Fantasy') < hits.indexOf('xfantasyx'), 'a word boundary beats the middle of a word');
  assert.ok(!hits.includes('Unfantastic') || hits.indexOf('Unfantastic') > hits.indexOf('Final Fantasy'));
});

test('equal scores keep the original order (an album stays in track order)', () => {
  const tracks = ['Album/10 Ten', 'Album/02 Two', 'Album/01 One'];
  assert.deepEqual(find(tracks, 'album'), tracks);
  assert.deepEqual(find(['song b', 'song a', 'song a extra'], 'song', 10), ['song b', 'song a', 'song a extra']);
});

test('matches report which characters matched, for highlighting', () => {
  const [hit] = search(['Final Fantasy'], 'fant', (x) => x);
  assert.deepEqual(hit.marks, [6, 7, 8, 9]);
  const [sub] = search(['Final Fantasy'], 'fnt', (x) => x); // f, n, t (tightest in-order run)
  assert.ok(sub.marks.length === 3 && sub.marks.every((i, k, a) => k === 0 || i > a[k - 1]));
  const [multi] = search(['Zelda Storms'], 'zel storm', (x) => x);
  assert.deepEqual(multi.marks, [0, 1, 2, 6, 7, 8, 9, 10]);
  const [typo] = search(['Final Fantasy'], 'fnial', (x) => x);
  assert.deepEqual(typo.marks, [0, 1, 2, 3, 4]); // the whole word that was matched
});

test('Japanese and other scripts are searched as written', () => {
  const items = ['夜に駆ける - YOASOBI', '紅蓮華 - LiSA', 'さくら'];
  assert.deepEqual(find(items, '紅蓮'), ['紅蓮華 - LiSA']);
  assert.deepEqual(find(items, 'yoasobi'), ['夜に駆ける - YOASOBI']);
  assert.deepEqual(find(items, 'ｓ'), []); // no accidental matches
});

test('nothing matches an empty or blank query, and nothing matches nonsense', () => {
  assert.deepEqual(find(['a', 'b'], ''), []);
  assert.deepEqual(find(['a', 'b'], '   '), []);
  assert.deepEqual(find(['Final Fantasy'], 'zzzzqqq'), []);
});

test('the result list is capped', () => {
  const items = Array.from({ length: 50 }, (_, i) => `track ${i}`);
  assert.equal(find(items, 'track', 7).length, 7);
  assert.equal(find(items, 'track').length, 50);
});

test('an index can be reused for many searches and returns the original objects', () => {
  const tracks = [
    { path: 'Zelda/ocarina.mp3', name: 'ocarina' },
    { path: 'Mario/overworld.mp3', name: 'overworld' },
  ];
  const index = createIndex(tracks, (t) => t.name);
  assert.equal(searchIndex(index, 'ocar')[0].item, tracks[0]);
  assert.equal(searchIndex(index, 'overw')[0].item, tracks[1]);
  assert.deepEqual(searchIndex(index, 'zzzz'), []);
});

test('fuzzyMatch can be used directly', () => {
  assert.equal(fuzzyMatch(['xyz'], 'abc def'), null);
  assert.ok(fuzzyMatch(['abc'], 'abc def').score > 0);
});

test('searching tens of thousands of tracks stays fast', () => {
  const words = ['final', 'fantasy', 'chrono', 'trigger', 'zelda', 'mario', 'sonic', 'metal', 'gear', 'solid', 'battle', 'theme', 'town', 'dungeon'];
  let seed = 12345; // deterministic pseudo-random library
  const pick = () => words[(seed = (seed * 1103515245 + 12345) % 2147483648) % words.length];
  const items = Array.from({ length: 50000 }, (_, i) => `${pick()} ${pick()} ${i} - ${pick()}`);
  const index = createIndex(items, (x) => x);
  const t0 = performance.now();
  const hits = searchIndex(index, 'fnial fantsy 4', 100);
  const ms = performance.now() - t0;
  assert.ok(hits.length > 0);
  assert.ok(ms < 1500, `search took ${ms.toFixed(0)} ms`);
});
