import test from 'node:test';
import assert from 'node:assert/strict';
import { UpNext } from '../../web/js/upnext.js';

const t = (path) => ({ path, name: path.split('/').pop() });
const paths = (u) => u.items.map((x) => x.path);

test('add appends new tracks, skips ones already listed, and numbers them from 1', () => {
  const u = new UpNext();
  assert.equal(u.add([t('a/1.mp3'), t('a/2.mp3')]), 2);
  assert.equal(u.add([t('a/2.mp3'), t('b/3.mp3'), t('b/3.mp3')]), 1);
  assert.deepEqual(paths(u), ['a/1.mp3', 'a/2.mp3', 'b/3.mp3']);
  assert.equal(u.position('a/1.mp3'), 1);
  assert.equal(u.position('b/3.mp3'), 3);
  assert.equal(u.position('nope'), 0);
});

test('playing through keeps every track listed and moves the cursor', () => {
  const u = new UpNext();
  u.add([t('1'), t('2')]);
  assert.equal(u.remaining, 2);
  assert.equal(u.next().path, '1');
  assert.equal(u.next().path, '2');
  assert.equal(u.next(), null);
  assert.equal(u.index, 1);
  assert.equal(u.remaining, 0);
  assert.deepEqual(paths(u), ['1', '2']);
  u.add([t('3')]); // added after playing through: plays next
  assert.equal(u.next().path, '3');
});

test('previous walks back to the start of the list, select jumps', () => {
  const u = new UpNext({ items: [t('1'), t('2'), t('3')] });
  assert.equal(u.previous(), null);
  assert.equal(u.select(2).path, '3');
  assert.equal(u.previous().path, '2');
  assert.equal(u.previous().path, '1');
  assert.equal(u.previous(), null);
  assert.equal(u.select(9), null);
  assert.equal(u.index, 0);
});

test('removing the cursor track makes the one that slid into its place play next', () => {
  const u = new UpNext({ items: [t('d/1'), t('d/2'), t('e/3'), t('d/4')], index: 1 });
  assert.equal(u.remove('d/2'), true);
  assert.equal(u.remove('d/2'), false);
  assert.equal(u.index, 0);
  assert.equal(u.next().path, 'e/3');
  assert.equal(u.removeWhere((x) => x.path.startsWith('d/')), 2);
  assert.deepEqual(paths(u), ['e/3']);
  assert.equal(u.index, 0);
  assert.equal(u.position('e/3'), 1);
  u.removeWhere(() => true);
  assert.equal(u.index, -1);
});

test('removing tracks after the cursor leaves it where it is', () => {
  const u = new UpNext({ items: [t('1'), t('2'), t('3')], index: 0 });
  u.remove('3');
  assert.equal(u.index, 0);
  assert.equal(u.next().path, '2');
});

test('clear returns the old list and replace brings it back, cursor and all', () => {
  const u = new UpNext({ items: [t('1'), t('2')], index: 0 });
  const old = u.clear();
  assert.equal(u.length, 0);
  assert.equal(u.index, -1);
  assert.deepEqual(u.clear().items, []);
  u.add([t('3'), t('1')]);
  u.replace(old);
  assert.deepEqual(paths(u), ['1', '2', '3']);
  assert.equal(u.index, 0);
});

test('the list goes idle after the set time without use; any edit or play restarts the clock', () => {
  let now = 1000;
  const u = new UpNext({}, () => now);
  assert.equal(u.idle(60), false); // empty is never idle
  u.add([t('1'), t('2')]);
  now += 59;
  assert.equal(u.idle(60), false);
  u.next();
  now += 59;
  assert.equal(u.idle(60), false);
  now += 1;
  assert.equal(u.idle(60), true);
  assert.equal(u.idle(0), false); // 0 = never
  u.touch();
  assert.equal(u.idle(60), false);
});

test('a saved list keeps its idle clock across a reload', () => {
  let now = 5000;
  const a = new UpNext({}, () => now);
  a.add([t('1')]);
  now += 100;
  const b = new UpNext(JSON.parse(JSON.stringify(a)), () => now);
  assert.equal(b.idle(100), true);
  assert.equal(b.touched, 5000);
});

test('change fires on every edit and not on no-ops', () => {
  const u = new UpNext();
  let n = 0;
  u.addEventListener('change', () => n++);
  u.add([t('1')]);
  u.add([t('1')]);
  u.remove('x');
  u.next();
  u.next();
  u.previous();
  u.clear();
  u.clear();
  assert.equal(n, 3);
});

test('rename and merge moves follow the listed tracks', () => {
  const u = new UpNext({ items: [t('old/1'), t('older/2'), t('x/3')], index: 1 });
  u.renameUnder('old', 'new');
  assert.deepEqual(paths(u), ['new/1', 'older/2', 'x/3']);
  u.remap({ 'x/3': 'y/3' });
  assert.equal(u.position('y/3'), 3);
  assert.equal(u.position('x/3'), 0);
  assert.equal(u.index, 1);
});

test('saved data: junk entries are dropped, the old plain-array format loads, a bad cursor is clamped', () => {
  const u = new UpNext({ items: [null, { path: 'a/1.mp3', fav: true }, { name: 'no path' }, { path: 'a/1.mp3' }], index: 7 });
  assert.deepEqual(u.items, [{ path: 'a/1.mp3', name: '1.mp3' }]);
  assert.equal(u.index, 0);
  const old = new UpNext([t('x/1')]);
  assert.deepEqual(paths(old), ['x/1']);
  assert.equal(old.index, -1);
  assert.equal(new UpNext(null).length, 0);
});
