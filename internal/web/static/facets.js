'use strict';

// Facet state lives in its own URL params, never in ?q= — the search input
// is never rewritten behind the user's back, and a hand-typed repo: atom
// composes with a facet selection instead of fighting it.
//
// This module never imports search.js: runSearch needs facetParams() and a
// toggle needs to re-search, which would be a cycle. main.js injects the
// re-search callback instead.

let facetsEl = null;
let onToggle = () => {};

const selected = { repo: new Set(), ext: new Set() };

// NO_EXT is the sentinel for files with no extension. The server reads an
// empty value as that bucket, so it round-trips as "?ext=".
const NO_EXT = '';

export function initFacets(opts) {
  facetsEl = opts.facetsEl;
  onToggle = opts.onToggle;
  readFacetsFromURL();
}

export function readFacetsFromURL() {
  const params = new URLSearchParams(location.search);
  for (const kind of ['repo', 'ext']) {
    selected[kind].clear();
    // Presence, not emptiness: "?ext=" selects the no-extension bucket.
    if (params.has(kind)) {
      for (const v of params.get(kind).split(',')) selected[kind].add(v);
    }
  }
}

// facetParams renders the selection as query params for /api/search.
export function facetParams() {
  const parts = [];
  for (const kind of ['repo', 'ext']) {
    if (selected[kind].size === 0) continue;
    parts.push(`&${kind}=${encodeURIComponent([...selected[kind]].join(','))}`);
  }
  return parts.join('');
}

function syncURL() {
  const url = new URL(location.href);
  for (const kind of ['repo', 'ext']) {
    if (selected[kind].size) url.searchParams.set(kind, [...selected[kind]].join(','));
    else url.searchParams.delete(kind);
  }
  // replaceState, matching how the query is synced: toggling should not
  // stack history entries.
  history.replaceState(null, '', url);
}

function toggle(kind, value) {
  if (selected[kind].has(value)) selected[kind].delete(value);
  else selected[kind].add(value);
  syncURL();
  // A click, not typing, so re-search immediately rather than waiting out
  // the input debounce.
  onToggle();
}

// activeCount is how many facet values are selected across all categories.
const activeCount = () => selected.repo.size + selected.ext.size;

// clearFacets drops every selection at once. Exported so a keyboard
// shortcut can reach it without going through the button.
export function clearFacets() {
  if (activeCount() === 0) return;
  selected.repo.clear();
  selected.ext.clear();
  syncURL();
  onToggle();
}

// renderFacets draws the panel from the server's facet universe, which is
// aggregated without the facet filters applied. Selected values are unioned
// in, so a value that the universe does not contain (a shared URL naming a
// repo the query does not match) still renders a chip that can be clicked
// to remove it.
export function renderFacets(facets) {
  if (!facetsEl) return;
  if (!facets) {
    facetsEl.replaceChildren();
    return;
  }
  const frag = document.createDocumentFragment();
  // Only shown when something is selected: with nothing active it would be
  // a permanently dead control.
  if (activeCount() > 0) frag.append(clearBar());
  frag.append(
    group('Repository', 'repo', facets.repos),
    group('File type', 'ext', facets.exts),
  );
  if (facets.truncated) {
    const note = document.createElement('p');
    note.className = 'facet-note';
    note.textContent = 'Some values may be missing — too many matches to scan.';
    frag.append(note);
  }
  facetsEl.replaceChildren(frag);
}

// clearBar reports how many filters are active and clears them all. Undoing
// a multi-category selection chip by chip re-runs a search per click, and
// with the sidebar hidden below the mobile breakpoint the only other way out
// is editing the URL.
function clearBar() {
  const bar = document.createElement('div');
  bar.className = 'facet-clear-bar';
  const n = activeCount();
  const count = document.createElement('span');
  count.textContent = `${n} ${n === 1 ? 'filter' : 'filters'}`;
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'facet-clear';
  b.textContent = 'Clear';
  b.title = 'Clear all filters (Shift+Backspace)';
  b.addEventListener('click', clearFacets);
  bar.append(count, b);
  return bar;
}

function group(title, kind, values) {
  const section = document.createElement('section');
  section.className = 'facet-group';
  const h = document.createElement('h3');
  h.textContent = title;
  section.append(h);

  const counts = new Map((values || []).map((v) => [v.value, v.count]));
  // Union of the universe and the selection, so a selection is always
  // deselectable even when it matches nothing.
  for (const v of selected[kind]) {
    if (!counts.has(v)) counts.set(v, 0);
  }
  if (counts.size === 0) {
    const empty = document.createElement('p');
    empty.className = 'facet-note';
    empty.textContent = 'No values.';
    section.append(empty);
    return section;
  }
  const list = document.createElement('div');
  list.className = 'facet-list';
  for (const [value, count] of counts) list.append(chip(kind, value, count));
  section.append(list);
  return section;
}

function chip(kind, value, count) {
  const b = document.createElement('button');
  b.type = 'button';
  const on = selected[kind].has(value);
  b.className = 'facet-chip' + (on ? ' on' : '');
  b.setAttribute('aria-pressed', on ? 'true' : 'false');

  const label = document.createElement('span');
  label.className = 'facet-value';
  label.textContent = value === NO_EXT && kind === 'ext' ? 'no extension' : value;
  const n = document.createElement('span');
  n.className = 'facet-count';
  n.textContent = count;
  b.append(label, n);
  b.title = `${label.textContent} · ${count}`;
  b.addEventListener('click', () => toggle(kind, value));
  return b;
}
