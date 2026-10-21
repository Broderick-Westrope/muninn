'use strict';

// Pure input parsing, matching and ordering for the command palette. No DOM
// and no state, so it can be tested under node.

import { parseLineRange } from './routes.js';

// PREFIXES are the palette's modes, by leading character. parse reads
// them and the palette footer lists them, so a new mode is added once.
export const PREFIXES = [
  { char: '>', mode: 'commands', label: 'commands' },
  { char: '@', mode: 'symbols', label: 'symbols' },
  { char: ':', mode: 'line', label: 'line' },
];

// parse splits palette input into its mode (by leading prefix) and term.
export function parse(input) {
  const v = input.trimStart();
  const p = PREFIXES.find((x) => x.char === v[0]);
  return p ? { mode: p.mode, term: v.slice(1).trim() } : { mode: 'all', term: v.trim() };
}

// splitLineTarget separates "payments.ts:15" into the file term and its
// line target. A bare ":15" is the go-to-line mode, not this. Forms met on
// the way to, or around, a target keep the file term so its lookup is not
// thrown away: a trailing colon ("main.go:"), a half-typed range
// ("main.go:10-", read as line 10), a line or range still being typed
// from zero ("main.go:0", "main.go:0-5"), and a trailing column as compilers print it
// ("main.go:10:5").
export function splitLineTarget(term) {
  const m = term.match(/^(.*?):\s*(\d+(?:\s*-\s*\d+)?)?\s*-?\s*(?::\d+)?$/);
  const base = m ? m[1].trim() : '';
  if (!m || !base) return { base: term, target: null };
  if (!m[2]) return { base, target: null };
  if (/^0+(\s*-|$)/.test(m[2])) return { base, target: null };
  const target = parseLineRange(m[2]);
  return target ? { base, target } : { base: term, target: null };
}

// escapeRe makes a typed token match literally: file: and sym: take regexes.
const escapeRe = (s) => s.replace(/[\\^$.|?*+()[\]{}"]/g, '\\$&');

// lookupQuery builds the zoekt query for a file-name or symbol lookup, each
// whitespace-separated token an ANDed literal. A repo:<regex> token passes
// through as zoekt's repo atom, so a name shared by many repos can be
// narrowed to one ("repo:pilot payments.ts"). File lookups ignore case,
// as the palette's other matching does: zoekt's smart case would otherwise
// make "README" miss readme.md. Symbols keep smart case, since a capital
// there usually means the exported name.
export function lookupQuery(kind, term) {
  const atom = kind === 'file' ? 'file' : 'sym';
  const parts = term
    .split(/\s+/)
    .filter(Boolean)
    .map((t) => (/^repo:./.test(t) ? t : `${atom}:${escapeRe(t)}`));
  return kind === 'file' ? `case:no ${parts.join(' ')} type:file` : parts.join(' ');
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
function fuzzy(text, term) {
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

// chord reports whether e is the app's primary modifier plus letter: Cmd on
// a Mac, Ctrl elsewhere, with no other modifiers. On a Mac, Ctrl+letter
// stays free for the text-editing bindings inputs give it there.
const chord = (e, mac, letter) =>
  (mac ? e.metaKey && !e.ctrlKey : e.ctrlKey && !e.metaKey) &&
  !e.altKey &&
  !e.shiftKey &&
  (e.key || '').toLowerCase() === letter;

// isPaletteKey is the open/close chord, Cmd+K or Ctrl+K.
export const isPaletteKey = (e, mac) => chord(e, mac, 'k');

// isGoToLineKey is the editor go-to-line chord, Cmd+G or Ctrl+G.
export const isGoToLineKey = (e, mac) => chord(e, mac, 'g');

// keyIntent maps a keydown in the palette input to an action, or null to
// let the key type. Keys during IME composition always type: their Enter
// and arrows confirm or move within the composed text. Ctrl+N/P navigate
// only on a Mac; elsewhere they are new-window and print.
export function keyIntent(e, mac) {
  if (e.isComposing || e.keyCode === 229) return null;
  if (isPaletteKey(e, mac)) return 'close';
  if (isGoToLineKey(e, mac)) return 'goToLine';
  const emacs = mac && e.ctrlKey && !e.metaKey && !e.altKey;
  if (e.key === 'ArrowDown' || (emacs && e.key === 'n')) return 'down';
  if (e.key === 'ArrowUp' || (emacs && e.key === 'p')) return 'up';
  if (e.key === 'Enter') return 'run';
  if (e.key === 'Escape') return 'close';
  return null;
}
