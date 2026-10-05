import test from 'node:test';
import assert from 'node:assert/strict';
import { loopPosition, loopRunTime } from '../../web/js/player.js';

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
