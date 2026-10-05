// Search state for the track lists: what to search, the cached index, the current hits and
// how to highlight them. No DOM, so it can be tested on its own.

import { createIndex, searchIndex } from '../lib/media-kit/fuzzy.js';
import { escapeHTML } from './util.js';

// The searched text of a track is "file name · folder / path", so artist and album folders
// match too. Highlight positions refer to this text; SEP is the width of the separator.
export const SEP = ' · ';

export function stemOf(name) {
  const i = name.lastIndexOf('.');
  return i > 0 ? name.slice(0, i) : name;
}

export function dirOf(path) {
  const i = path.lastIndexOf('/');
  return i < 0 ? '' : path.slice(0, i).split('/').join(' / ');
}

export const searchText = (e) => `${stemOf(e.name)}${SEP}${dirOf(e.path)}`;

// text with the characters at marks (shifted by offset) wrapped in <mark>, HTML-escaped.
export function highlight(text, marks, offset = 0) {
  const on = new Set(marks.map((m) => m - offset));
  let out = '';
  let open = false;
  for (let i = 0; i < text.length; i++) {
    const m = on.has(i);
    if (m && !open) out += '<mark>';
    if (!m && open) out += '</mark>';
    open = m;
    out += escapeHTML(text[i]);
  }
  return open ? `${out}</mark>` : out;
}

export class SearchState {
  constructor() {
    this.query = '';
    this.hits = [];
    this.pool = null;
    this.index = null;
    this.markMap = new Map();
  }

  get active() {
    return this.query !== '';
  }

  // Searches pool (the tracks of the current list). The index is rebuilt only when the pool
  // itself changes (a different array: another list, or after an upload or delete).
  run(pool, query, limit = 300) {
    this.query = query.trim();
    if (!this.active) return this.clear();
    if (pool !== this.pool) {
      this.index = createIndex(pool, searchText);
      this.pool = pool;
    }
    this.hits = searchIndex(this.index, this.query, limit);
    this.markMap = new Map(this.hits.map((h) => [h.item.path, h.marks]));
    return this.hits;
  }

  clear() {
    this.query = '';
    this.hits = [];
    this.markMap = new Map();
    return this.hits;
  }

  items() {
    return this.hits.map((h) => h.item);
  }

  marksFor(path) {
    return this.markMap.get(path) || null;
  }

  // A track disappeared (deleted, or un-favorited in the favorites list).
  remove(path) {
    this.hits = this.hits.filter((h) => h.item.path !== path);
    this.markMap.delete(path);
  }
}
