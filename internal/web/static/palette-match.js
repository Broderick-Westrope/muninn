'use strict';

// Pure input parsing, matching and ordering for the command palette. No DOM
// and no state, so it can be tested under node.

import { parseLineRange } from './routes.js';

// parse splits palette input into its mode (by leading prefix) and term.
export function parse(v) {
  const c = v[0];
  if (c === '>') return { mode: 'commands', term: v.slice(1).trim() };
  if (c === '@') return { mode: 'symbols', term: v.slice(1).trim() };
  if (c === ':') return { mode: 'line', term: v.slice(1).trim() };
  return { mode: 'all', term: v.trim() };
}

// splitLineTarget separates "payments.ts:15" into the file term and its
// line target. A bare ":15" is the go-to-line mode, not this. A trailing
// colon (typed on the way to "main.go:15") is dropped rather than searched.
export function splitLineTarget(term) {
  const at = term.lastIndexOf(':');
  if (at <= 0) return { base: term, target: null };
  const base = term.slice(0, at).trim();
  const rest = term.slice(at + 1).trim();
  if (!rest && base) return { base, target: null };
  const target = parseLineRange(rest);
  return target && base ? { base, target } : { base: term, target: null };
}

// escapeRe makes a typed token match literally: file: and sym: take regexes.
const escapeRe = (s) => s.replace(/[\\^$.|?*+()[\]{}"]/g, '\\$&');

// lookupQuery builds the zoekt query for a file-name or symbol lookup, each
// whitespace-separated token an ANDed literal. A repo:<regex> token passes
// through as zoekt's repo atom, so a name shared by many repos can be
// narrowed to one ("repo:pilot payments.ts").
export function lookupQuery(kind, term) {
  const atom = kind === 'file' ? 'file' : 'sym';
  const parts = term
    .split(/\s+/)
    .filter(Boolean)
    .map((t) => (/^repo:./.test(t) ? t : `${atom}:${escapeRe(t)}`));
  return kind === 'file' ? `${parts.join(' ')} type:file` : parts.join(' ');
}

// mergeFiles puts the current repo's matches ahead of the global ones,
// dropping duplicates. The global lookup is capped, so on a common name it
// can miss the current repo entirely; a scoped lookup guarantees it a place.
export function mergeFiles(scoped, global) {
  const seen = new Set();
  const out = [];
  for (const f of [...scoped, ...global]) {
    const k = `${f.repo}\u0000${f.path}`;
    if (seen.has(k)) continue;
    seen.add(k);
    out.push(f);
  }
  return out;
}


// fuzzyFilter keeps the items whose title matches term, best first.
export function fuzzyFilter(list, term) {
  if (!term) return list;
  const scored = [];
  for (const it of list) {
    const m = fuzzy(it.title, term);
    if (m) scored.push({ ...it, positions: m.positions, score: m.score });
  }
  return scored.sort((a, b) => b.score - a.score);
}

// fuzzy matches term case-insensitively as a substring of text, or else as
// a run of word prefixes ("gts" for "Go to symbol"): every character must
// continue the previous match or start a word. Letters scattered mid-word
// would match nearly every long repo name, so they do not count. A
// substring outranks word prefixes, and earlier matches outrank later ones.
export function fuzzy(text, term) {
  const t = text.toLowerCase();
  const q = term.toLowerCase().replace(/\s+/g, ' ');
  const at = t.indexOf(q);
  if (at !== -1) {
    const positions = [];
    for (let i = at; i < at + q.length; i++) positions.push(i);
    return { score: 1000 + (isWordStart(t, at) ? 100 : 0) - at, positions };
  }
  const positions = [];
  let score = 0;
  let from = 0;
  for (const ch of q) {
    if (ch === ' ') continue;
    let j = -1;
    // Continue the current word if possible, else jump to the next word
    // that starts with ch.
    if (positions.length && t[from] === ch) {
      j = from;
    } else {
      for (let k = from; k < t.length && j === -1; k++) {
        if (t[k] === ch && isWordStart(t, k)) j = k;
      }
    }
    if (j === -1) return null;
    score += j === from && positions.length ? 5 : 8;
    positions.push(j);
    from = j + 1;
  }
  return { score: score - (positions[positions.length - 1] - positions[0]), positions };
}

const isWordStart = (t, i) => i === 0 || /[\s/._\-:]/.test(t[i - 1]);

// substringPositions marks each token's first occurrence in text, for
// highlighting server-matched results.
export function substringPositions(text, term) {
  const t = text.toLowerCase();
  const out = [];
  for (const tok of term.toLowerCase().split(/\s+/)) {
    const at = tok ? t.indexOf(tok) : -1;
    for (let i = at; at !== -1 && i < at + tok.length; i++) out.push(i);
  }
  return out;
}

// isGoToLineKey is Cmd+G on a Mac and Ctrl+G elsewhere, the editor
// go-to-line chord.
export const isGoToLineKey = (e, mac) =>
  (mac ? e.metaKey : e.ctrlKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'g';

// keyIntent maps a keydown in the palette input to an action, or null to
// let the key type. Keys during IME composition always type: their Enter
// and arrows confirm or move within the composed text.
export function keyIntent(e, mac) {
  if (e.isComposing || e.keyCode === 229) return null;
  const ctrlOnly = e.ctrlKey && !e.metaKey && !e.altKey;
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') return 'close';
  if (isGoToLineKey(e, mac)) return 'goToLine';
  if (e.key === 'ArrowDown' || (ctrlOnly && e.key === 'n')) return 'down';
  if (e.key === 'ArrowUp' || (ctrlOnly && e.key === 'p')) return 'up';
  if (e.key === 'Enter') return 'run';
  if (e.key === 'Escape') return 'close';
  return null;
}

