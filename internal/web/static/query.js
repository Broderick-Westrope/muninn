'use strict';

// ---------------------------------------------------------------------------
// Query token extraction (best-effort client-side match highlighting: the
// search API returns full line text without offsets)

// bareTokens extracts plain-word tokens from a zoekt query: atoms
// (repo:, file:, sym:, ...), negations, and boolean operators are skipped;
// quoted strings are used literally; tokens containing regex
// metacharacters are skipped (their literal text likely never appears).
export function bareTokens(q) {
  const tokens = [];
  const re = /"([^"]*)"|(\S+)/g;
  let m;
  while ((m = re.exec(q))) {
    if (m[1] !== undefined) {
      if (m[1]) tokens.push(m[1]);
      continue;
    }
    const t = m[2];
    if (t.startsWith('-')) continue;
    if (/^[a-z_]+:/i.test(t)) continue;
    if (/^(and|or)$/i.test(t)) continue;
    if (/[\\^$.|?*+()[\]{}]/.test(t)) continue;
    tokens.push(t);
  }
  return tokens;
}

// ---------------------------------------------------------------------------
// Syntax hint chips

const CHIPS = [
  ['repo:', 'limit to repos matching a regex'],
  ['file:', 'limit to file paths matching a regex'],
  ['sym:', 'match symbol definitions'],
  ['lang:', 'limit to a language'],
  ['case:yes', 'case-sensitive matching'],
  ['-', 'negate the next atom'],
  ['"..."', 'exact phrase'],
];

// initHints builds the chip row, each chip inserting its atom into the
// search input at the caret.
export function initHints(hintsEl, searchInput) {
  for (const [atom, title] of CHIPS) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'chip';
    b.textContent = atom;
    b.title = title;
    b.addEventListener('click', () => insertAtom(searchInput, atom));
    hintsEl.append(b);
  }
}

function insertAtom(searchInput, atom) {
  const insert = atom === '"..."' ? '""' : atom;
  const v = searchInput.value;
  const at = searchInput.selectionStart ?? v.length;
  const before = v.slice(0, at);
  const pad = before && !before.endsWith(' ') ? ' ' : '';
  searchInput.value = before + pad + insert + v.slice(at);
  const caret = at + pad.length + (atom === '"..."' ? 1 : insert.length);
  searchInput.focus();
  searchInput.setSelectionRange(caret, caret);
}
