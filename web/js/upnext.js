// Up next: a temporary list of tracks picked while browsing or searching, like foobar2000's
// playback queue. Its tracks play in order before the play queue carries on. Played tracks stay
// in the list (a cursor marks how far it got); the whole list clears itself once it has gone
// unused for a set time. Pure logic, no DOM; fires 'change' on every edit.

export class UpNext extends EventTarget {
  // saved: what toJSON() returned (or a plain array of tracks, the old format).
  constructor(saved = {}, now = Date.now) {
    super();
    this.now = now;
    this.items = [];
    this.index = -1; // the last track started from this list; the next one plays next
    this._pos = new Map(); // path -> 1-based position, for the row badges
    const s = Array.isArray(saved) ? { items: saved } : saved || {};
    this._set(Array.isArray(s.items) ? s.items : [], Number.isInteger(s.index) ? s.index : -1);
    this.touched = Number.isFinite(s.touched) ? s.touched : this.now();
  }

  get length() {
    return this.items.length;
  }

  // Tracks after the cursor, still to play.
  get remaining() {
    return this.items.length - this.index - 1;
  }

  // 1-based position of a track in the list, or 0 when it is not in it.
  position(path) {
    return this._pos.get(path) || 0;
  }

  // Appends tracks that are not listed yet. Returns how many were added.
  add(tracks) {
    const before = this.items.length;
    this._set([...this.items, ...tracks], this.index);
    const n = this.items.length - before;
    if (n) this._changed();
    return n;
  }

  remove(path) {
    return this.removeWhere((t) => t.path === path) > 0;
  }

  // Removes every track the predicate picks. Returns how many went. If the cursor's track goes,
  // the cursor moves back one, so the track that slid into its place plays next.
  removeWhere(pred) {
    const before = this.items.length;
    let index = -1;
    const kept = [];
    this.items.forEach((t, i) => {
      if (pred(t)) return;
      kept.push(t);
      if (i <= this.index) index = kept.length - 1;
    });
    if (kept.length === before) return 0;
    this._set(kept, index);
    this._changed();
    return before - kept.length;
  }

  // Moves the cursor on and returns the track to play, or null when the list is played through.
  next() {
    if (this.index + 1 >= this.items.length) return null;
    return this.select(this.index + 1);
  }

  // Moves the cursor back and returns that track, or null at the start of the list.
  previous() {
    if (this.index <= 0) return null;
    return this.select(this.index - 1);
  }

  // Puts the cursor on track i (it is about to play) and returns it.
  select(i) {
    const t = this.items[i];
    if (!t) return null;
    this.index = i;
    this._changed();
    return t;
  }

  // Empties the list. Returns what it held, for replace() to bring back (Undo).
  clear() {
    const old = this.toJSON();
    if (!old.items.length) return old;
    this._set([], -1);
    this._changed();
    return old;
  }

  // Restores a list from clear() or toJSON(). Tracks added since are kept, after it.
  replace(saved) {
    this._set([...saved.items, ...this.items], saved.index ?? -1);
    this._changed();
  }

  // True when the list has gone unused (nothing added, nothing played from it) for ttlMs.
  // ttlMs <= 0 means never.
  idle(ttlMs) {
    return ttlMs > 0 && this.items.length > 0 && this.now() - this.touched >= ttlMs;
  }

  // Marks the list as in use, which restarts the idle clock.
  touch() {
    this.touched = this.now();
  }

  // A folder was renamed: point the listed tracks inside it at the new path.
  renameUnder(from, to) {
    this._set(this.items.map((t) => (t.path.startsWith(`${from}/`) ? { ...t, path: to + t.path.slice(from.length) } : t)), this.index);
    this._changed();
  }

  // Files moved (a folder merge): point listed tracks at where their content is now.
  remap(moves) {
    this._set(this.items.map((t) => (Object.hasOwn(moves, t.path) ? { ...t, path: moves[t.path] } : t)), this.index);
    this._changed();
  }

  toJSON() {
    return { items: this.items.slice(), index: this.index, touched: this.touched };
  }

  // Keeps the first copy of each path, so the list never holds a track twice.
  _set(items, index) {
    this.items = [];
    this._pos.clear();
    for (const t of items) {
      if (!t?.path || this._pos.has(t.path)) continue;
      this.items.push({ path: t.path, name: t.name ?? t.path.split('/').pop() });
      this._pos.set(t.path, this.items.length);
    }
    this.index = Math.min(Math.max(index, -1), this.items.length - 1);
  }

  _changed() {
    this.touch();
    this.dispatchEvent(new Event('change'));
  }
}
