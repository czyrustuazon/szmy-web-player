// Fuzzy search. Pure functions, no DOM.
//
// A query is split into words and every word must match (AND). A word matches a text in
// one of three ways, best first:
//   1. as a substring ("fant" in "Final Fantasy"), with bonuses for starting at a word
//      boundary or the start of the text;
//   2. as an in-order subsequence of letters ("fnlfntsy" for "Final Fantasy"), only for
//      words of 3+ letters so that short queries do not match everything;
//   3. with a typo: one slip in a word of 4-7 letters, two in 8+ (a substitution, a missing or
//      extra letter, or two swapped letters: "fnial" for "final"), checked against whole
//      words and, so a half-typed word still works, against their beginnings.
//
// Matching is case-insensitive and ignores accents. Normalisation keeps the text the same
// length, so match positions (for highlighting) line up with the original string.

const SCORE_SUBSTRING = 100;
const SCORE_SUBSEQUENCE = 50;
const SCORE_TYPO = 40;
const MAX_TOKENS = 8;

// Lower-cases and strips accents one character at a time (1:1, so indexes are preserved).
export function normalize(s) {
  let out = '';
  for (let i = 0; i < s.length; i++) {
    const ch = s[i];
    const lower = ch.normalize('NFD')[0].toLowerCase(); // é -> e, then lower-case
    out += lower.length === 1 ? lower : ch; // keep the original if the width would change
  }
  return out;
}

const isWordChar = (c) => c !== undefined && /[\p{L}\p{N}]/u.test(c);
const isBoundary = (text, i) => i === 0 || !isWordChar(text[i - 1]);

function words(text) {
  const out = [];
  let start = -1;
  for (let i = 0; i <= text.length; i++) {
    const w = i < text.length && isWordChar(text[i]);
    if (w && start < 0) start = i;
    if (!w && start >= 0) {
      out.push({ text: text.slice(start, i), start });
      start = -1;
    }
  }
  return out;
}

// Optimal string alignment distance (Levenshtein plus adjacent transpositions), giving up
// once it exceeds max.
export function editDistance(a, b, max) {
  if (Math.abs(a.length - b.length) > max) return max + 1;
  let prev2 = null;
  let prev = Array.from({ length: b.length + 1 }, (_, j) => j);
  for (let i = 1; i <= a.length; i++) {
    const cur = [i];
    let rowMin = i;
    for (let j = 1; j <= b.length; j++) {
      const cost = a[i - 1] === b[j - 1] ? 0 : 1;
      let v = Math.min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + cost);
      if (i > 1 && j > 1 && a[i - 1] === b[j - 2] && a[i - 2] === b[j - 1]) v = Math.min(v, prev2[j - 2] + 1);
      cur[j] = v;
      if (v < rowMin) rowMin = v;
    }
    if (rowMin > max) return max + 1;
    prev2 = prev;
    prev = cur;
  }
  return prev[b.length];
}

const range = (start, len) => Array.from({ length: len }, (_, k) => start + k);

function matchSubstring(t, text) {
  let best = null;
  for (let i = text.indexOf(t); i >= 0; i = text.indexOf(t, i + 1)) {
    let score = SCORE_SUBSTRING + t.length * 10;
    if (isBoundary(text, i)) score += 30;
    if (i === 0) score += 20;
    if (!isWordChar(text[i + t.length])) score += 15; // ends a word: "fan" in "fan club", not "fantasy"
    if (!best || score > best.score) best = { score, marks: range(i, t.length) };
  }
  return best;
}

function matchSubsequence(t, text) {
  if (t.length < 3) return null;
  // Forward pass finds where a match can end; the backward pass then tightens its start.
  let ti = 0;
  let end = -1;
  for (let i = 0; i < text.length && ti < t.length; i++) {
    if (text[i] === t[ti]) {
      ti++;
      end = i;
    }
  }
  if (ti < t.length) return null;
  const marks = new Array(t.length);
  ti = t.length - 1;
  for (let i = end; i >= 0 && ti >= 0; i--) {
    if (text[i] === t[ti]) marks[ti--] = i;
  }
  const span = end - marks[0] + 1;
  if (span > t.length * 3 + 4) return null; // too scattered to be what the user meant
  let score = SCORE_SUBSEQUENCE + t.length * 8 - (span - t.length) * 2;
  for (let k = 0; k < marks.length; k++) {
    if (isBoundary(text, marks[k])) score += 10;
    if (k > 0 && marks[k] === marks[k - 1] + 1) score += 5;
  }
  return { score, marks };
}

function matchTypo(t, ws) {
  const max = t.length >= 8 ? 2 : t.length >= 4 ? 1 : 0;
  if (max === 0) return null;
  let best = null;
  for (const w of ws) {
    // Compare with the whole word, and with its beginning (a half-typed word).
    for (const cand of w.text.length > t.length ? [w.text, w.text.slice(0, t.length)] : [w.text]) {
      const d = editDistance(t, cand, max);
      if (d > max) continue;
      const score = SCORE_TYPO + t.length * 6 - d * 15 + (w.start === 0 ? 10 : 0);
      if (!best || score > best.score) best = { score, marks: range(w.start, cand.length) };
    }
  }
  return best;
}

function matchToken(t, text, ws) {
  return matchSubstring(t, text) || matchSubsequence(t, text) || matchTypo(t, ws);
}

// Scores one (already normalised) text against the query tokens; null if any token fails.
export function fuzzyMatch(tokens, text, ws = words(text)) {
  let score = 0;
  const marks = new Set();
  for (const t of tokens) {
    const m = matchToken(t, text, ws);
    if (!m) return null;
    score += m.score;
    for (const i of m.marks) marks.add(i);
  }
  return { score, marks: [...marks].sort((a, b) => a - b) };
}

export function tokenize(query) {
  return normalize(query).split(/\s+/).filter(Boolean).slice(0, MAX_TOKENS);
}

// Pre-normalises the searchable text of every item once, so searching stays fast on big
// libraries. textOf(item) is what to search in.
export function createIndex(items, textOf) {
  return items.map((item) => {
    const raw = textOf(item);
    const text = normalize(raw);
    return { item, raw, text, words: words(text) };
  });
}

// Best matches first: [{item, score, marks}]. An empty query matches nothing.
export function searchIndex(index, query, limit = 200) {
  const tokens = tokenize(query);
  if (!tokens.length) return [];
  const hits = [];
  index.forEach((entry, i) => {
    const m = fuzzyMatch(tokens, entry.text, entry.words);
    if (m) hits.push({ item: entry.item, score: m.score, marks: m.marks, i });
  });
  // Equal scores keep the original order, so an album's tracks stay in track order.
  hits.sort((a, b) => b.score - a.score || a.i - b.i);
  return hits.slice(0, limit).map(({ item, score, marks }) => ({ item, score, marks }));
}

export function search(items, query, textOf, limit) {
  return searchIndex(createIndex(items, textOf), query, limit);
}
