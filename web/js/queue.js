// Play queue: order, shuffle bag, repeat modes. Pure logic, no DOM.
//
// Mirrors szmy: shuffle draws from a no-repeat bag that refills when empty,
// repeat is off / all / one, and auto-advance crosses folder boundaries
// because the queue is the whole library (or the favorites list).

export const REPEAT = ['off', 'all', 'one'];

export class Queue {
  constructor(rand = Math.random) {
    this.rand = rand;
    this.items = [];
    this.index = -1;
    this.shuffle = false;
    this.repeat = 'off';
    this.bag = []; // indices not yet played this shuffle cycle
    this.history = []; // indices played, for "previous" while shuffling
  }

  get length() {
    return this.items.length;
  }

  current() {
    return this.items[this.index] || null;
  }

  peekNext() {
    // Best-effort guess used for prefetching; never mutates state.
    if (!this.items.length) return null;
    if (this.shuffle) return this.bag.length ? this.items[this.bag[this.bag.length - 1]] : null;
    return this.items[this.index + 1] || null;
  }

  setQueue(items, startIndex = 0) {
    this.items = items.slice();
    this.index = this.items.length ? Math.min(Math.max(startIndex, 0), this.items.length - 1) : -1;
    this.history = [];
    this._refill();
  }

  setShuffle(on) {
    this.shuffle = !!on;
    this.history = [];
    this._refill();
  }

  setRepeat(mode) {
    this.repeat = REPEAT.includes(mode) ? mode : 'off';
  }

  cycleRepeat() {
    this.repeat = REPEAT[(REPEAT.indexOf(this.repeat) + 1) % REPEAT.length];
    return this.repeat;
  }

  select(path) {
    const i = this.items.findIndex((t) => t.path === path);
    if (i < 0) return null;
    this._goto(i);
    return this.items[i];
  }

  // Next track. auto=true means the previous one ended by itself.
  // Returns null when playback should stop.
  next(auto = false) {
    if (!this.items.length) return null;
    if (auto && this.repeat === 'one') return this.current();
    let i;
    if (this.shuffle) {
      if (!this.bag.length) {
        if (auto && this.repeat !== 'all') return null;
        this._refill();
      }
      i = this.bag.pop();
      if (i === undefined) i = this.index; // single-item library
    } else if (this.index + 1 < this.items.length) {
      i = this.index + 1;
    } else if (this.repeat === 'all' || !auto) {
      i = 0;
    } else {
      return null;
    }
    this._push(i);
    return this.items[i];
  }

  previous() {
    if (!this.items.length) return null;
    let i;
    if (this.shuffle && this.history.length) {
      i = this.history.pop();
      this.bag = this.bag.filter((b) => b !== i);
      this.index = i;
      return this.items[i];
    }
    i = this.index > 0 ? this.index - 1 : this.items.length - 1;
    this.index = i;
    return this.items[i];
  }

  // Remove a track (it was deleted). Returns {wasCurrent, next} where next
  // is the track that should play now if the current one was removed.
  remove(path) {
    const i = this.items.findIndex((t) => t.path === path);
    if (i < 0) return { wasCurrent: false, next: null };
    this.items.splice(i, 1);
    const fix = (n) => (n > i ? n - 1 : n);
    this.bag = this.bag.filter((b) => b !== i).map(fix);
    this.history = this.history.filter((h) => h !== i).map(fix);
    const wasCurrent = i === this.index;
    if (i < this.index) this.index -= 1;
    if (!this.items.length) {
      this.index = -1;
      return { wasCurrent, next: null };
    }
    if (!wasCurrent) return { wasCurrent, next: null };
    // The track that slid into this slot plays next; when shuffling, draw from the bag.
    if (this.shuffle && this.bag.length) {
      this.index = this.bag.pop();
    } else {
      this.index = Math.min(i, this.items.length - 1);
      this.bag = this.bag.filter((b) => b !== this.index);
    }
    return { wasCurrent, next: this.items[this.index] };
  }

  // Remove every track inside a deleted folder. Same result shape as remove().
  removeUnder(dir) {
    const out = { wasCurrent: false, next: null };
    for (const t of this.items.filter((x) => x.path.startsWith(`${dir}/`))) {
      const r = this.remove(t.path);
      if (r.wasCurrent) {
        out.wasCurrent = true;
        out.next = r.next;
      }
    }
    return out;
  }

  // A folder was renamed: point the queued tracks inside it at the new path.
  renameUnder(from, to) {
    for (const t of this.items) {
      if (t.path.startsWith(`${from}/`)) t.path = to + t.path.slice(from.length);
    }
  }

  // Files moved (a folder merge): point queued tracks at where their content is now.
  remap(moves) {
    for (const t of this.items) {
      if (Object.hasOwn(moves, t.path)) t.path = moves[t.path];
    }
  }

  _goto(i) {
    this._push(i);
  }

  _push(i) {
    if (this.index >= 0 && this.index !== i) this.history.push(this.index);
    if (this.history.length > 500) this.history.shift();
    this.index = i;
    this.bag = this.bag.filter((b) => b !== i);
  }

  // Rebuild the shuffle bag: every index except the current one, shuffled.
  _refill() {
    this.bag = [];
    if (!this.shuffle) return;
    for (let i = 0; i < this.items.length; i++) if (i !== this.index) this.bag.push(i);
    for (let i = this.bag.length - 1; i > 0; i--) {
      const j = Math.floor(this.rand() * (i + 1));
      [this.bag[i], this.bag[j]] = [this.bag[j], this.bag[i]];
    }
  }
}
