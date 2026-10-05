import test from 'node:test';
import assert from 'node:assert/strict';
import { Queue } from '../../web/js/queue.js';

const tracks = (n) => Array.from({ length: n }, (_, i) => ({ path: `t${i}` }));

test('sequential next walks the list and stops at the end when auto', () => {
  const q = new Queue();
  q.setQueue(tracks(3), 0);
  assert.equal(q.next(true).path, 't1');
  assert.equal(q.next(true).path, 't2');
  assert.equal(q.next(true), null);
});

test('manual next wraps at the end, repeat-all wraps automatically', () => {
  const q = new Queue();
  q.setQueue(tracks(2), 1);
  assert.equal(q.next(false).path, 't0');
  q.setQueue(tracks(2), 1);
  q.setRepeat('all');
  assert.equal(q.next(true).path, 't0');
});

test('repeat-one replays on auto-advance but manual next still moves', () => {
  const q = new Queue();
  q.setQueue(tracks(3), 1);
  q.setRepeat('one');
  assert.equal(q.next(true).path, 't1');
  assert.equal(q.next(false).path, 't2');
});

test('repeat cycles off -> all -> one -> off', () => {
  const q = new Queue();
  assert.equal(q.cycleRepeat(), 'all');
  assert.equal(q.cycleRepeat(), 'one');
  assert.equal(q.cycleRepeat(), 'off');
  q.setRepeat('bogus');
  assert.equal(q.repeat, 'off');
});

test('shuffle plays every track once per cycle before repeating', () => {
  const q = new Queue(() => 0.3);
  q.setQueue(tracks(6), 0);
  q.setShuffle(true);
  const seen = new Set([q.current().path]);
  for (let i = 0; i < 5; i++) seen.add(q.next(true).path);
  assert.equal(seen.size, 6);
  assert.equal(q.next(true), null); // cycle done, repeat off
  q.setRepeat('all');
  assert.ok(q.next(true));
});

test('shuffle previous walks back through history', () => {
  const q = new Queue(() => 0.5);
  q.setQueue(tracks(5), 0);
  q.setShuffle(true);
  const a = q.next(true).path;
  const b = q.next(true).path;
  assert.equal(q.previous().path, a);
  assert.notEqual(a, b);
});

test('previous in order wraps to the last track', () => {
  const q = new Queue();
  q.setQueue(tracks(3), 0);
  assert.equal(q.previous().path, 't2');
  assert.equal(q.previous().path, 't1');
});

test('remove of the current track advances to the track in its slot', () => {
  const q = new Queue();
  q.setQueue(tracks(3), 1);
  const r = q.remove('t1');
  assert.equal(r.wasCurrent, true);
  assert.equal(r.next.path, 't2');
  assert.equal(q.current().path, 't2');
});

test('remove of the last current track moves to the new last track', () => {
  const q = new Queue();
  q.setQueue(tracks(3), 2);
  const r = q.remove('t2');
  assert.equal(r.next.path, 't1');
});

test('remove of an earlier track keeps the current one current', () => {
  const q = new Queue();
  q.setQueue(tracks(4), 2);
  const r = q.remove('t0');
  assert.equal(r.wasCurrent, false);
  assert.equal(q.current().path, 't2');
});

test('remove of the only track empties the queue', () => {
  const q = new Queue();
  q.setQueue(tracks(1), 0);
  const r = q.remove('t0');
  assert.equal(r.next, null);
  assert.equal(q.current(), null);
  assert.equal(q.next(false), null);
});

test('remove of an unknown path is a no-op', () => {
  const q = new Queue();
  q.setQueue(tracks(2), 0);
  assert.deepEqual(q.remove('nope'), { wasCurrent: false, next: null });
});

test('remove while shuffling never leaves stale bag indices', () => {
  const q = new Queue(() => 0.1);
  q.setQueue(tracks(5), 0);
  q.setShuffle(true);
  q.remove('t3');
  q.remove('t0');
  const seen = new Set();
  while (true) {
    const t = q.next(true);
    if (!t) break;
    assert.ok(q.items.includes(t));
    seen.add(t.path);
  }
  assert.ok(!seen.has('t3') && !seen.has('t0'));
});

test('select jumps to a path and drops it from the bag', () => {
  const q = new Queue();
  q.setQueue(tracks(4), 0);
  q.setShuffle(true);
  assert.equal(q.select('t2').path, 't2');
  assert.ok(!q.bag.includes(2));
  assert.equal(q.select('missing'), null);
});

test('empty queue is safe', () => {
  const q = new Queue();
  assert.equal(q.next(true), null);
  assert.equal(q.previous(), null);
  assert.equal(q.current(), null);
  assert.equal(q.peekNext(), null);
});

test('removeUnder drops a folder and hands off when the current track was inside it', () => {
  const q = new Queue();
  q.setQueue([{ path: 'a/1' }, { path: 'a/2' }, { path: 'b/1' }], 0);
  const r = q.removeUnder('a');
  assert.equal(r.wasCurrent, true);
  assert.equal(r.next.path, 'b/1');
  assert.equal(q.length, 1);
  assert.equal(q.removeUnder('zzz').wasCurrent, false);
});

test('renameUnder re-points queued tracks and ignores look-alike prefixes', () => {
  const q = new Queue();
  q.setQueue([{ path: 'a/1' }, { path: 'ab/1' }], 0);
  q.renameUnder('a', 'c');
  assert.deepEqual(q.items.map((t) => t.path), ['c/1', 'ab/1']);
});

test('remap re-points moved tracks, only those that moved', () => {
  const q = new Queue();
  q.setQueue([{ path: 'a/1.mp3' }, { path: 'a/2.mp3' }, { path: 'b/3.mp3' }, { path: 'toString' }], 0);
  q.remap({ 'a/1.mp3': 'b/1.mp3', 'a/2.mp3': 'b/2 (2).mp3' });
  assert.deepEqual(q.items.map((t) => t.path), ['b/1.mp3', 'b/2 (2).mp3', 'b/3.mp3', 'toString']);
});
