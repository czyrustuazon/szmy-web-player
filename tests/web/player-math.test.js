import test from 'node:test';
import assert from 'node:assert/strict';
import { loopPosition, loopRunTime } from '../../web/js/player.js';
import { bandEdges, decay } from '../../web/js/viz.js';

test('loopPosition is linear before the loop end', () => {
  assert.equal(loopPosition(5, 10, 60), 5);
  assert.equal(loopPosition(59.9, 10, 60), 59.9);
});

test('loopPosition wraps back into the loop section', () => {
  assert.equal(loopPosition(60, 10, 60), 10);
  assert.equal(loopPosition(65, 10, 60), 15);
  assert.equal(loopPosition(110, 10, 60), 10);
});

test('loopRunTime counts the first pass to the loop end plus extra passes', () => {
  // intro 10s, loop 10..60 (50s): 2 plays of the loop = 60 + 50 seconds from start
  assert.equal(loopRunTime(0, 10, 60, 2), 110);
  assert.equal(loopRunTime(0, 10, 60, 1), 60);
  // seeking into the loop only shortens the first pass
  assert.equal(loopRunTime(40, 10, 60, 2), 70);
  // a count below 1 is treated as 1
  assert.equal(loopRunTime(0, 10, 60, 0), 60);
});

test('bandEdges are increasing, within range and span 80 Hz..12 kHz', () => {
  const edges = bandEdges(44100, 1024, 24);
  assert.equal(edges.length, 25);
  for (let i = 1; i < edges.length; i++) assert.ok(edges[i] >= edges[i - 1]);
  assert.ok(edges[0] >= 0 && edges[edges.length - 1] <= 1023);
  const hz = (bin) => (bin / 1023) * 22050;
  assert.ok(Math.abs(hz(edges[0]) - 80) < 30);
  assert.ok(Math.abs(hz(edges[24]) - 12000) < 30);
});

test('decay drops by half plus 8 and floors at zero', () => {
  assert.equal(decay(100), 42);
  assert.equal(decay(10), 0);
  assert.equal(decay(0), 0);
});
