'use strict';

import { api } from './api.js';
import { createLookup } from './palette-lookup.js';
import {
  fuzzyFilter,
  isGoToLineKey as isGoToLineKeyFor,
  keepActive,
  keyIntent,
  lookupQuery,
  mergeFiles,
  parse,
  shouldQueueEnter,
  splitLineTarget,
  substringPositions,
} from './palette-match.js';
import { fileHash, lineSuffix, parseLineRange } from './routes.js';

// The command palette (Cmd/Ctrl+K). Plain text mixes commands, recent and
// matching files, repos, and a code search; a leading ">" narrows to
// commands, "@" to symbol definitions, and ":" to a line in the open file.
// A trailing ":<line>" or ":<start>-<end>" on a file lookup
// ("payments.ts:15") opens the chosen file at that line, as in an editor's
// quick open.
//
// This module owns no app state. main.js injects the context-aware command
// list and the search actions, so the palette never imports the modules
// whose commands it shows.

const REMOTE_LIMIT = 30;
const REMOTE_DEBOUNCE_MS = 120;
const REMOTE_MIN_CHARS = 2;
const MAX_COMMANDS_MIXED = 6; // commands shown alongside files and repos
const MAX_REPOS = 5;
const MAX_FILES = 12;
const MAX_SYMBOLS = 25;
const RECENT_KEY = 'muninn.recentFiles';
const RECENT_MAX = 8;
const TOAST_MS = 1400;

export const isMac = /Mac|iPhone|iPad/.test(navigator.platform);

let opts = null;
let dlg = null;
let input = null;
let listEl = null;
let toastEl = null;
let toastTimer = 0;

let items = []; // selectable items in render order
let active = 0;
let repoNames = null; // cached /api/repos names, loaded on first open

// navigated is set once the user moves the selection after typing; an
// Enter before that, while a lookup is in flight, waits for its results
// instead of running whatever happens to be on top.
let navigated = false;
let enterQueued = false;
let lookupStart = Infinity; // index where the lookup's rows begin in items

const lookup = createLookup({
  fetch: fetchLookup,
  debounceMs: REMOTE_DEBOUNCE_MS,
  onSettle: ({ ok }) => {
    if (!paletteOpen()) return;
    update(!navigated);
    if (!enterQueued) return;
    enterQueued = false;
    // On failure the fallback on top is not what Enter was waiting for;
    // stay open with the error showing instead.
    if (ok && items[active]) runItem(items[active]);
  },
});

// initPalette builds the dialog. o supplies:
//   commands()        context-aware command list ({title, detail, keys,
//                     run, stay, toast}); stay re-opens the palette with
//                     that prefix instead of running anything
//   currentFile()     the showing file location, or null
//   search(q)         run a code search
//   searchRepo(repo)  narrow the search to one repo
export function initPalette(o) {
  opts = o;
  dlg = document.createElement('dialog');
  dlg.className = 'palette';
  dlg.setAttribute('aria-label', 'Command palette');

  input = document.createElement('input');
  input.type = 'text';
  input.className = 'pal-input';
  input.placeholder = 'Type a command, file[:line], or repo — > commands, @ symbols, : line';
  input.autocomplete = 'off';
  input.spellcheck = false;
  input.setAttribute('autocapitalize', 'off');
  input.setAttribute('role', 'combobox');
  input.setAttribute('aria-expanded', 'true');
  input.setAttribute('aria-controls', 'pal-list');

  listEl = document.createElement('ul');
  listEl.id = 'pal-list';
  listEl.className = 'pal-list';
  listEl.setAttribute('role', 'listbox');

  const foot = document.createElement('div');
  foot.className = 'pal-foot';
  for (const [k, label] of [
    ['↑↓', 'navigate'],
    ['↵', 'run'],
    ['esc', 'close'],
    ['>', 'commands'],
    ['@', 'symbols'],
    [':', 'line'],
  ]) {
    const kbd = document.createElement('kbd');
    kbd.textContent = k;
    foot.append(kbd, ` ${label}`);
  }

  dlg.append(input, listEl, foot);
  document.body.append(dlg);

  toastEl = document.createElement('div');
  toastEl.className = 'toast';
  toastEl.setAttribute('role', 'status');
  toastEl.hidden = true;
  document.body.append(toastEl);

  input.addEventListener('input', () => {
    navigated = false;
    enterQueued = false;
    update(true);
  });
  input.addEventListener('keydown', onKey);
  listEl.addEventListener('mousemove', (e) => {
    const li = e.target.closest('li[data-i]');
    if (li && Number(li.dataset.i) !== active) {
      navigated = true;
      enterQueued = false;
      setActive(Number(li.dataset.i), false);
    }
  });
  listEl.addEventListener('click', (e) => {
    const li = e.target.closest('li[data-i]');
    if (li) runItem(items[Number(li.dataset.i)]);
  });
  // A click on the backdrop lands on the dialog element itself.
  dlg.addEventListener('click', (e) => {
    if (e.target === dlg) dlg.close();
  });
  dlg.addEventListener('close', () => {
    lookup.cancel();
    enterQueued = false;
  });
}

export const paletteOpen = () => Boolean(dlg && dlg.open);

// isGoToLineKey is Cmd+G (Ctrl+G off a Mac), the editor go-to-line chord.
export const isGoToLineKey = (e) => isGoToLineKeyFor(e, isMac);

export function togglePalette() {
  if (paletteOpen()) dlg.close();
  else openPalette('');
}

export function openPalette(prefix) {
  if (!dlg.open) dlg.showModal();
  input.value = prefix;
  input.focus();
  navigated = false;
  enterQueued = false;
  update(true);
  if (!repoNames) loadRepoNames();
}

// rememberFile records a viewed file for the palette's recent list.
export function rememberFile(loc) {
  const recent = readRecent().filter((r) => r.repo !== loc.repo || r.path !== loc.path);
  recent.unshift({ repo: loc.repo, path: loc.path });
  try {
    localStorage.setItem(RECENT_KEY, JSON.stringify(recent.slice(0, RECENT_MAX)));
  } catch {
    /* storage unavailable (private mode) — recents are a nicety */
  }
}

function readRecent() {
  try {
    const v = JSON.parse(localStorage.getItem(RECENT_KEY) || '[]');
    return Array.isArray(v) ? v.filter((r) => r && r.repo && r.path) : [];
  } catch {
    return [];
  }
}

async function loadRepoNames() {
  try {
    repoNames = (await api('/api/repos')).map((r) => r.name);
  } catch {
    repoNames = null; // retried on the next open
    return;
  }
  if (paletteOpen()) update(!navigated);
}

// ---------------------------------------------------------------------------
// Building the list

// update rebuilds the list. reset moves the selection to the top. Async
// re-renders (results arriving) pass reset only until the user has moved
// the selection: after that they keep it, so Enter still runs what the
// user picked.
function update(reset = true) {
  const { mode, term } = parse(input.value);
  const groups = [];
  let wantsLookup = false;
  if (mode === 'line') {
    groups.push(lineGroup(term));
  } else if (mode === 'symbols') {
    wantsLookup = true;
    groups.push(remoteGroup('Symbols', 'sym', term, MAX_SYMBOLS));
  } else if (mode === 'commands') {
    groups.push({ label: 'Commands', items: fuzzyFilter(opts.commands(), term) });
  } else if (!term) {
    groups.push({ label: 'Recent files', items: recentItems() });
    groups.push({ label: 'Commands', items: opts.commands() });
  } else {
    wantsLookup = true;
    const { base, target } = splitLineTarget(term);
    const files = remoteGroup('Files', 'file', base, MAX_FILES);
    if (target) {
      // A line target says the user wants a file, so files lead and the
      // chosen one opens at that line.
      const atLine = (it) => ({
        ...it,
        title: it.title + lineSuffix(target.line, target.endLine),
        run: () => goTo(it.repo, it.path, target.line, target.endLine),
      });
      files.items = files.items.map(atLine);
      files.stale = files.stale.map(atLine);
      groups.push(files);
    } else {
      groups.push({
        label: 'Commands',
        items: fuzzyFilter(opts.commands(), term).slice(0, MAX_COMMANDS_MIXED),
      });
      groups.push(files);
      groups.push({ label: 'Repositories', items: repoItems(term) });
    }
    groups.push({
      label: 'Search',
      items: [{ title: `Search code for “${term}”`, run: () => opts.search(term) }],
    });
  }
  // A mode without a lookup must not leave an older one able to fire a
  // queued Enter or re-render the list.
  if (!wantsLookup) lookup.view('', null);
  render(groups, reset);
}

function recentItems() {
  const cur = opts.currentFile();
  return readRecent()
    .filter((r) => !cur || r.repo !== cur.repo || r.path !== cur.path)
    .map((r) => ({ title: r.path, detail: r.repo, run: () => goTo(r.repo, r.path) }));
}

function repoItems(term) {
  if (!repoNames) return [];
  const list = repoNames.map((name) => ({
    title: name,
    detail: 'search in repo',
    run: () => opts.searchRepo(name),
  }));
  return fuzzyFilter(list, term).slice(0, MAX_REPOS);
}

function lineGroup(term) {
  const f = opts.currentFile();
  if (!f) return { label: 'Go to line', items: [], note: 'Open a file first.' };
  const t = parseLineRange(term);
  if (!t) return { label: 'Go to line', items: [], note: 'Type a line number, or a range like 10-20.' };
  const title = t.endLine ? `Go to lines ${t.line}–${t.endLine}` : `Go to line ${t.line}`;
  return {
    label: 'Go to line',
    items: [{ title, detail: f.path, run: () => goTo(f.repo, f.path, t.line, t.endLine) }],
  };
}

function goTo(repo, path, line, endLine) {
  const hash = fileHash(repo, path, line, endLine);
  // Re-choosing the current target fires no hashchange, but the user may
  // have scrolled away from it: route anyway so it scrolls back.
  if (location.hash === hash) window.dispatchEvent(new HashChangeEvent('hashchange'));
  else location.hash = hash;
}

// remoteGroup returns what the lookup knows for a file or symbol term right
// now. Results from an older term are passed as stale: shown, dimmed, so
// the list does not flicker, but never selectable.
function remoteGroup(label, kind, term, max) {
  if (term.length < REMOTE_MIN_CHARS) {
    lookup.view('', null);
    return { label, items: [], stale: [], note: kind === 'sym' ? 'Type a symbol name.' : '' };
  }
  // The current repo is part of the key: its scoped results differ per repo.
  const repo = opts.currentFile()?.repo || '';
  const v = lookup.view(`${kind}\u0000${repo}\u0000${term}`, { kind, term, repo });
  const sameKind = v.staleKey.startsWith(`${kind}\u0000`);
  const group = {
    label,
    lookup: true,
    items: v.items.slice(0, max),
    stale: sameKind ? v.stale.slice(0, max) : [],
    note: '',
  };
  if (v.error) group.note = v.error;
  else if (v.pending) group.note = 'Searching…';
  else if (!group.items.length) group.note = 'No matches.';
  else if (v.truncated || v.items.length > max) group.note = 'More matches — keep typing, or add repo:<name>.';
  return group;
}

// fetchLookup runs a file or symbol lookup. In the file view it also asks
// for the current repo alone: the global request is capped, so a common
// name (index.ts) could otherwise crowd the current repo out entirely.
async function fetchLookup({ kind, term, repo }, signal) {
  const q = `/api/search?q=${encodeURIComponent(lookupQuery(kind, term))}&limit=${REMOTE_LIMIT}`;
  const [global, scoped] = await Promise.all([
    api(q, signal),
    repo ? api(`${q}&repo=${encodeURIComponent(repo)}`, signal) : null,
  ]);
  const files = mergeFiles(scoped ? scoped.files : [], global.files);
  return {
    items: kind === 'file' ? fileItems(files, term, repo) : symbolItems(files, term),
    truncated: global.truncated || Boolean(scoped && scoped.truncated),
  };
}

function fileItems(files, term, repo) {
  return files
    .map((f, i) => ({ f, i }))
    // Within the current repo and within the rest, shorter paths first;
    // the merge already put the current repo ahead.
    .sort((a, b) => (b.f.repo === repo) - (a.f.repo === repo) || a.f.path.length - b.f.path.length)
    .map(({ f }) => ({
      title: f.path,
      positions: substringPositions(f.path, term),
      detail: f.repo,
      repo: f.repo,
      path: f.path,
      run: () => goTo(f.repo, f.path),
    }));
}

function symbolItems(files, term) {
  const out = [];
  for (const f of files) {
    for (const l of f.lines) {
      if (!l.isSymbolDef) continue;
      const text = l.line.trim();
      out.push({
        title: text,
        positions: substringPositions(text, term),
        detail: `${f.repo}/${f.path}:${l.lineNumber}`,
        run: () => goTo(f.repo, f.path, l.lineNumber),
      });
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// Rendering and keys

function render(groups, reset) {
  const prevKey = items[active]?.key;
  items = [];
  lookupStart = Infinity;
  const frag = document.createDocumentFragment();
  for (const g of groups) {
    const stale = g.stale || [];
    if (!g.items.length && !stale.length && !g.note) continue;
    const head = document.createElement('li');
    head.className = 'pal-group';
    head.setAttribute('role', 'presentation');
    head.textContent = g.label;
    frag.append(head);
    if (g.lookup) lookupStart = items.length;
    for (const it of g.items) {
      const keyed = { ...it, key: `${g.label}\u0000${it.title}\u0000${it.detail || ''}` };
      frag.append(itemEl(keyed, items.push(keyed) - 1));
    }
    for (const it of stale) frag.append(itemEl(it, -1));
    if (g.note) {
      const n = document.createElement('li');
      n.className = 'pal-note';
      n.setAttribute('role', 'presentation');
      n.textContent = g.note;
      frag.append(n);
    }
  }
  if (!frag.childNodes.length) {
    const n = document.createElement('li');
    n.className = 'pal-note';
    n.setAttribute('role', 'presentation');
    n.textContent = 'Nothing matches.';
    frag.append(n);
  }
  listEl.replaceChildren(frag);
  active = keepActive(prevKey, items.map((it) => it.key), reset);
  setActive(active, reset);
}

// itemEl renders one row; i is its index among selectable items, or -1 for
// a stale row that is shown but cannot be chosen.
function itemEl(it, i) {
  const li = document.createElement('li');
  if (i < 0) {
    li.className = 'pal-item stale';
    li.setAttribute('role', 'presentation');
  } else {
    li.className = 'pal-item';
    li.id = `pal-${i}`;
    li.dataset.i = i;
    li.setAttribute('role', 'option');
  }
  const title = document.createElement('span');
  title.className = 'pal-title';
  highlight(title, it.title, it.positions);
  li.append(title);
  if (it.detail) {
    const d = document.createElement('span');
    d.className = 'pal-detail';
    d.textContent = it.detail;
    li.append(d);
  }
  if (it.keys) {
    const k = document.createElement('span');
    k.className = 'pal-keys';
    for (const key of it.keys) {
      const kbd = document.createElement('kbd');
      kbd.textContent = key;
      k.append(kbd);
    }
    li.append(k);
  }
  return li;
}

// highlight renders text with the matched character positions marked.
// Built from text nodes; user data never goes through innerHTML.
function highlight(el, text, positions) {
  if (!positions || !positions.length) {
    el.textContent = text;
    return;
  }
  const set = new Set(positions);
  let run = '';
  let marked = false;
  const flush = () => {
    if (!run) return;
    if (marked) {
      const m = document.createElement('mark');
      m.textContent = run;
      el.append(m);
    } else {
      el.append(run);
    }
    run = '';
  };
  for (let i = 0; i < text.length; i++) {
    const on = set.has(i);
    if (on !== marked) {
      flush();
      marked = on;
    }
    run += text[i];
  }
  flush();
}

function setActive(i, reset) {
  for (const el of listEl.querySelectorAll('[aria-selected="true"]')) {
    el.setAttribute('aria-selected', 'false');
  }
  active = items.length ? (i + items.length) % items.length : 0;
  const el = $item(active);
  if (!el) {
    input.removeAttribute('aria-activedescendant');
    return;
  }
  el.setAttribute('aria-selected', 'true');
  input.setAttribute('aria-activedescendant', el.id);
  if (reset) listEl.scrollTop = 0;
  else el.scrollIntoView({ block: 'nearest' });
}

const $item = (i) => (items.length ? document.getElementById(`pal-${i}`) : null);

function onKey(e) {
  // Keys typed here must never reach the page's global shortcuts ("/",
  // Esc back to results, Shift+Backspace clearing filters).
  e.stopPropagation();
  const intent = keyIntent(e, isMac);
  if (!intent) return;
  e.preventDefault();
  if (intent === 'close') {
    dlg.close();
  } else if (intent === 'goToLine') {
    // Also swallowed without a file open: the browser's find-next would
    // otherwise fire behind the modal.
    if (opts.currentFile()) openPalette(':');
  } else if (intent === 'down' || intent === 'up') {
    // Moving the selection is a new choice: a queued Enter was for the old
    // one and must not fire on whatever is selected when results land.
    navigated = true;
    enterQueued = false;
    setActive(active + (intent === 'down' ? 1 : -1), false);
  } else if (intent === 'run') {
    // Typed fast and hit Enter: wait for the lookup the input asked for
    // rather than run a row from before it arrived.
    if (shouldQueueEnter({ pending: lookup.pending(), navigated, active, lookupStart })) {
      enterQueued = true;
    } else if (items[active]) {
      runItem(items[active]);
    }
  }
}

// runItem closes the palette before running, so a command that moves focus
// (focusing the search box) is not undone by the dialog restoring focus.
async function runItem(it) {
  if (it.stay !== undefined) {
    input.value = it.stay;
    input.focus();
    navigated = false;
    update(true);
    return;
  }
  dlg.close();
  try {
    await it.run();
  } catch (e) {
    showToast(e.message || 'Command failed');
    return;
  }
  if (it.toast) showToast(it.toast);
}

function showToast(msg) {
  toastEl.textContent = msg;
  toastEl.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => {
    toastEl.hidden = true;
  }, TOAST_MS);
}
