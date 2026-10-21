'use strict';

import { api } from './api.js';
import { fileHash } from './routes.js';

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

// The last remote (file or symbol) lookup. Its items stay on screen while a
// newer lookup is in flight, so typing does not flicker the list.
let remote = { key: '', kind: '', items: [], error: '' };
let remoteCtl = null;
let remoteTimer = 0;
let pendingKey = ''; // lookup scheduled or in flight; re-renders must not restart it

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

  input.addEventListener('input', update);
  input.addEventListener('keydown', onKey);
  listEl.addEventListener('mousemove', (e) => {
    const li = e.target.closest('li[data-i]');
    if (li && Number(li.dataset.i) !== active) setActive(Number(li.dataset.i), false);
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
    clearTimeout(remoteTimer);
    if (remoteCtl) remoteCtl.abort();
    pendingKey = '';
  });
}

export const paletteOpen = () => Boolean(dlg && dlg.open);

// isGoToLineKey is Cmd+G (Ctrl+G off a Mac), the editor go-to-line chord.
export const isGoToLineKey = (e) =>
  (isMac ? e.metaKey : e.ctrlKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'g';

export function togglePalette() {
  if (paletteOpen()) dlg.close();
  else openPalette('');
}

export function openPalette(prefix) {
  if (!dlg.open) dlg.showModal();
  input.value = prefix;
  input.focus();
  update();
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
  if (paletteOpen()) update();
}

// ---------------------------------------------------------------------------
// Building the list

function parse(v) {
  const c = v[0];
  if (c === '>') return { mode: 'commands', term: v.slice(1).trim() };
  if (c === '@') return { mode: 'symbols', term: v.slice(1).trim() };
  if (c === ':') return { mode: 'line', term: v.slice(1).trim() };
  return { mode: 'all', term: v.trim() };
}

// parseLines reads "15" or "10-20" as a line target, normalising a
// reversed range; null when s is not a line target.
function parseLines(s) {
  const m = s.match(/^(\d+)(?:\s*-\s*(\d+))?$/);
  if (!m) return null;
  let line = Number(m[1]);
  let endLine = m[2] ? Number(m[2]) : 0;
  if (endLine && endLine < line) [line, endLine] = [endLine, line];
  if (endLine === line) endLine = 0;
  return line > 0 ? { line, endLine } : null;
}

// splitLineTarget separates "payments.ts:15" into the file term and its
// line target. A bare ":15" is the go-to-line mode, not this.
function splitLineTarget(term) {
  const at = term.lastIndexOf(':');
  if (at <= 0) return { base: term, target: null };
  const target = parseLines(term.slice(at + 1).trim());
  const base = term.slice(0, at).trim();
  return target && base ? { base, target } : { base: term, target: null };
}

const lineSuffix = (t) => (t.endLine ? `:${t.line}-${t.endLine}` : `:${t.line}`);

function update() {
  const { mode, term } = parse(input.value);
  const groups = [];
  if (mode === 'line') {
    groups.push(lineGroup(term));
  } else if (mode === 'symbols') {
    groups.push(remoteGroup('Symbols', 'sym', term, MAX_SYMBOLS));
  } else if (mode === 'commands') {
    groups.push({ label: 'Commands', items: fuzzyFilter(opts.commands(), term) });
  } else if (!term) {
    groups.push({ label: 'Recent files', items: recentItems() });
    groups.push({ label: 'Commands', items: opts.commands() });
  } else {
    const { base, target } = splitLineTarget(term);
    const files = remoteGroup('Files', 'file', base, MAX_FILES);
    if (target) {
      // A line target says the user wants a file, so files lead and the
      // chosen one opens at that line.
      files.items = files.items.map((it) => ({
        ...it,
        title: it.title + lineSuffix(target),
        run: () => goTo(it.repo, it.path, target.line, target.endLine),
      }));
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
  render(groups);
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
  const t = parseLines(term);
  if (!t) return { label: 'Go to line', items: [], note: 'Type a line number, or a range like 10-20.' };
  const title = t.endLine ? `Go to lines ${t.line}–${t.endLine}` : `Go to line ${t.line}`;
  return {
    label: 'Go to line',
    items: [{ title, detail: f.path, run: () => goTo(f.repo, f.path, t.line, t.endLine) }],
  };
}

function goTo(repo, path, line, endLine) {
  location.hash = fileHash(repo, path, line, endLine);
}

// remoteGroup returns what is known for a file or symbol lookup right now,
// scheduling the request when the term has changed.
function remoteGroup(label, kind, term, max) {
  if (term.length < REMOTE_MIN_CHARS) {
    return { label, items: [], note: kind === 'sym' ? 'Type a symbol name.' : '' };
  }
  const key = `${kind}\u0000${term}`;
  if (remote.key !== key && pendingKey !== key) scheduleRemote(kind, term, key);
  const fresh = remote.key === key;
  const shown = remote.kind === kind ? remote.items.slice(0, max) : [];
  let note = '';
  if (fresh && remote.error) note = remote.error;
  else if (fresh && shown.length === 0) note = 'No matches.';
  else if (!fresh && shown.length === 0) note = 'Searching…';
  return { label, items: shown, note };
}

function scheduleRemote(kind, term, key) {
  clearTimeout(remoteTimer);
  if (remoteCtl) remoteCtl.abort();
  pendingKey = key;
  remoteTimer = setTimeout(async () => {
    remoteCtl = new AbortController();
    const tokens = term.split(/\s+/).map(escapeRe);
    const q =
      kind === 'file'
        ? tokens.map((t) => `file:${t}`).join(' ') + ' type:file'
        : tokens.map((t) => `sym:${t}`).join(' ');
    let next;
    try {
      const data = await api(
        `/api/search?q=${encodeURIComponent(q)}&limit=${REMOTE_LIMIT}`,
        remoteCtl.signal,
      );
      next = { key, kind, items: kind === 'file' ? fileItems(data, term) : symbolItems(data, term), error: '' };
    } catch (e) {
      if (e.name === 'AbortError') return;
      next = { key, kind, items: [], error: e.message };
    }
    pendingKey = '';
    remote = next;
    if (paletteOpen()) update();
  }, REMOTE_DEBOUNCE_MS);
}

// escapeRe makes a typed token match literally: file: and sym: take regexes.
const escapeRe = (s) => s.replace(/[\\^$.|?*+()[\]{}"]/g, '\\$&');

// The current repo sorts first: a lookup while reading code is most often
// for a neighbour.
function byCurrentRepo(a, b) {
  const repo = opts.currentFile()?.repo;
  return (b.repo === repo) - (a.repo === repo);
}

function fileItems(data, term) {
  return data.files
    .map((f) => ({ repo: f.repo, path: f.path }))
    .sort((a, b) => byCurrentRepo(a, b) || a.path.length - b.path.length)
    .map((f) => ({
      title: f.path,
      positions: substringPositions(f.path, term),
      detail: f.repo,
      repo: f.repo,
      path: f.path,
      run: () => goTo(f.repo, f.path),
    }));
}

function symbolItems(data, term) {
  const out = [];
  for (const f of [...data.files].sort(byCurrentRepo)) {
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
// Matching

// fuzzyFilter keeps the items whose title matches term, best first.
function fuzzyFilter(list, term) {
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

function substringPositions(text, term) {
  const t = text.toLowerCase();
  const out = [];
  for (const tok of term.toLowerCase().split(/\s+/)) {
    const at = tok ? t.indexOf(tok) : -1;
    for (let i = at; at !== -1 && i < at + tok.length; i++) out.push(i);
  }
  return out;
}

// ---------------------------------------------------------------------------
// Rendering and keys

function render(groups) {
  items = [];
  const frag = document.createDocumentFragment();
  for (const g of groups) {
    if (!g.items.length && !g.note) continue;
    const head = document.createElement('li');
    head.className = 'pal-group';
    head.setAttribute('role', 'presentation');
    head.textContent = g.label;
    frag.append(head);
    for (const it of g.items) frag.append(itemEl(it, items.push(it) - 1));
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
    n.textContent = 'Nothing matches.';
    frag.append(n);
  }
  listEl.replaceChildren(frag);
  setActive(0, true);
}

function itemEl(it, i) {
  const li = document.createElement('li');
  li.className = 'pal-item';
  li.id = `pal-${i}`;
  li.dataset.i = i;
  li.setAttribute('role', 'option');
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
  $item(active)?.setAttribute('aria-selected', 'false');
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
  const ctrlOnly = e.ctrlKey && !e.metaKey && !e.altKey;
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
    e.preventDefault();
    dlg.close();
  } else if (isGoToLineKey(e)) {
    // Also swallowed without a file open: the browser's find-next would
    // otherwise fire behind the modal.
    e.preventDefault();
    if (opts.currentFile()) openPalette(':');
  } else if (e.key === 'ArrowDown' || (ctrlOnly && e.key === 'n')) {
    e.preventDefault();
    setActive(active + 1, false);
  } else if (e.key === 'ArrowUp' || (ctrlOnly && e.key === 'p')) {
    e.preventDefault();
    setActive(active - 1, false);
  } else if (e.key === 'Enter') {
    e.preventDefault();
    if (items[active]) runItem(items[active]);
  } else if (e.key === 'Escape') {
    e.preventDefault();
    dlg.close();
  }
}

// runItem closes the palette before running, so a command that moves focus
// (focusing the search box) is not undone by the dialog restoring focus.
async function runItem(it) {
  if (it.stay !== undefined) {
    input.value = it.stay;
    input.focus();
    update();
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
