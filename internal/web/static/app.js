'use strict';

const $ = (sel, el = document) => el.querySelector(sel);

const searchInput = $('#q');
const hintsEl = $('#hints');
const resultsEl = $('#results');
const fileEl = $('#file');
const statsEl = $('#stats');
const repoBadge = $('#repo-badge');

const SEARCH_LIMIT = 100;
const DEBOUNCE_MS = 200;
const COPY_FEEDBACK_MS = 1000;

let debounceTimer = 0;
let searchCtl = null; // AbortController for the in-flight search
let fileCtl = null; // AbortController for the in-flight file fetch
let currentFile = null; // {repo, path, line} when the file view is showing
let currentFileData = null; // /api/file response for the showing file
let savedScroll = 0; // results scroll position, restored on return

// ---------------------------------------------------------------------------
// API

async function api(path, signal) {
  const res = await fetch(path, { signal });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      msg = (await res.json()).error || msg;
    } catch {
      /* non-JSON error body */
    }
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  return res.json();
}

// ---------------------------------------------------------------------------
// Query token extraction (best-effort client-side match highlighting: the
// search API returns full line text without offsets)

// bareTokens extracts plain-word tokens from a zoekt query: atoms
// (repo:, file:, sym:, ...), negations, and boolean operators are skipped;
// quoted strings are used literally; tokens containing regex
// metacharacters are skipped (their literal text likely never appears).
function bareTokens(q) {
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

// markMatches renders line text into el, wrapping case-insensitive
// occurrences of tokens in <mark>. Built with text nodes, never innerHTML.
function markMatches(el, text, tokens) {
  const lower = text.toLowerCase();
  const ranges = [];
  for (const t of tokens) {
    const lt = t.toLowerCase();
    let i = 0;
    while (lt && (i = lower.indexOf(lt, i)) !== -1) {
      ranges.push([i, i + lt.length]);
      i += lt.length;
    }
  }
  ranges.sort((a, b) => a[0] - b[0]);
  const merged = [];
  for (const r of ranges) {
    const last = merged[merged.length - 1];
    if (last && r[0] <= last[1]) last[1] = Math.max(last[1], r[1]);
    else merged.push(r);
  }
  let pos = 0;
  for (const [start, end] of merged) {
    if (start > pos) el.append(text.slice(pos, start));
    const mark = document.createElement('mark');
    mark.textContent = text.slice(start, end);
    el.append(mark);
    pos = end;
  }
  if (pos < text.length) el.append(text.slice(pos));
}

// ---------------------------------------------------------------------------
// Copy affordances (inline SVG, currentColor so themes work; icon markup is
// static — user data still flows through textContent everywhere)

const ICONS = {
  copy:
    '<path fill="none" stroke="currentColor" stroke-width="1.5" d="M5.75 5.75h7.5a1 1 0 0 1 1 1v7.5a1 1 0 0 1-1 1h-7.5a1 1 0 0 1-1-1v-7.5a1 1 0 0 1 1-1Z"/>' +
    '<path fill="none" stroke="currentColor" stroke-width="1.5" d="M10.25 5.75v-3a1 1 0 0 0-1-1h-6.5a1 1 0 0 0-1 1v6.5a1 1 0 0 0 1 1h3"/>',
  check: '<path fill="none" stroke="currentColor" stroke-width="2" d="m2.75 8.75 3.5 3.5 7-8.5"/>',
  link:
    '<path fill="none" stroke="currentColor" stroke-width="1.5" d="M6.5 9.5a3 3 0 0 0 4.4.2l2.2-2.2a3 3 0 1 0-4.3-4.2L7.6 4.5"/>' +
    '<path fill="none" stroke="currentColor" stroke-width="1.5" d="M9.5 6.5a3 3 0 0 0-4.4-.2L2.9 8.5a3 3 0 1 0 4.3 4.2l1.2-1.2"/>',
};

function icon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 16 16');
  svg.setAttribute('aria-hidden', 'true');
  svg.innerHTML = ICONS[name]; // static markup from ICONS only, never user data
  return svg;
}

// copyBtn returns an icon button that copies text (a string, or a function
// evaluated at click time) and briefly swaps to a checkmark to confirm.
function copyBtn(text, title, iconName = 'copy') {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'copy-btn';
  b.title = title;
  b.setAttribute('aria-label', title);
  b.append(icon(iconName));
  let timer = 0;
  b.addEventListener('click', async (e) => {
    // Copy buttons live inside result-row anchors; never navigate.
    e.preventDefault();
    e.stopPropagation();
    try {
      await navigator.clipboard.writeText(typeof text === 'function' ? text() : text);
    } catch {
      return; // clipboard unavailable — leave the button inert
    }
    b.replaceChildren(icon('check'));
    b.classList.add('copied');
    clearTimeout(timer);
    timer = setTimeout(() => {
      b.replaceChildren(icon(iconName));
      b.classList.remove('copied');
    }, COPY_FEEDBACK_MS);
  });
  return b;
}

// ---------------------------------------------------------------------------
// Search

function fileHash(repo, path, line) {
  const p = path.split('/').map(encodeURIComponent).join('/');
  return `#/file/${repo}/${p}${line ? ':' + line : ''}`;
}

async function runSearch(q, fromUser) {
  const url = new URL(location.href);
  if (q) url.searchParams.set('q', q);
  else url.searchParams.delete('q');
  history.replaceState(null, '', url);

  if (searchCtl) searchCtl.abort();
  if (!q) {
    resultsEl.replaceChildren(note('Type to search across all indexed repos.'));
    statsEl.hidden = true;
    return;
  }
  searchCtl = new AbortController();
  let data;
  try {
    data = await api(
      `/api/search?q=${encodeURIComponent(q)}&limit=${SEARCH_LIMIT}`,
      searchCtl.signal,
    );
  } catch (e) {
    if (e.name === 'AbortError') return;
    statsEl.hidden = true;
    resultsEl.replaceChildren(errorBox(e.message));
    return;
  }
  renderResults(data, bareTokens(q));
  // Searching from the file view returns to results — but only for
  // user-initiated searches, never the initial background one (which
  // would yank a deep-linked file view away).
  if (fromUser && location.hash.startsWith('#/file/')) location.hash = '#/';
}

function renderResults(data, tokens) {
  const groups = new Map();
  for (const f of data.files) {
    if (!groups.has(f.repo)) groups.set(f.repo, []);
    groups.get(f.repo).push(f);
  }
  const frag = document.createDocumentFragment();
  if (data.files.length === 0) frag.append(note('No matches.'));
  for (const [repo, files] of groups) {
    const section = document.createElement('section');
    section.className = 'repo-group';
    const h = document.createElement('h2');
    h.textContent = repo;
    section.append(h);
    for (const f of files) section.append(fileCard(f, tokens));
    frag.append(section);
  }
  resultsEl.replaceChildren(frag);
  renderStats(data);
}

function fileCard(f, tokens) {
  const card = document.createElement('article');
  card.className = 'card';
  const head = document.createElement('div');
  head.className = 'card-head';
  const path = document.createElement('a');
  path.className = 'card-path';
  path.href = fileHash(f.repo, f.path, f.lines[0]?.lineNumber);
  path.textContent = f.path;
  head.append(path, copyBtn(`${f.repo}/${f.path}`, 'Copy repo/path'));
  card.append(head);
  for (const l of f.lines) {
    const row = document.createElement('a');
    row.className = 'row' + (l.isSymbolDef ? ' symdef' : '');
    row.href = fileHash(f.repo, f.path, l.lineNumber);
    const num = document.createElement('span');
    num.className = 'num';
    num.textContent = l.lineNumber;
    const code = document.createElement('code');
    markMatches(code, l.line, tokens);
    row.append(num, code, copyBtn(`${f.repo}/${f.path}:${l.lineNumber}`, 'Copy repo/path:line'));
    card.append(row);
  }
  return card;
}

function renderStats(data) {
  const s = data.stats;
  const files = data.files.length;
  statsEl.hidden = false;
  statsEl.replaceChildren(
    `${s.matchCount} ${plural(s.matchCount, 'match', 'matches')} in ` +
      `${files} ${plural(files, 'file', 'files')} · ` +
      `${s.filesConsidered} considered · ${s.durationMs}ms`,
  );
  if (data.truncated) {
    const t = document.createElement('span');
    t.className = 'trunc';
    t.textContent = 'truncated — refine your query';
    statsEl.append(' · ', t);
  }
}

const plural = (n, one, many) => (n === 1 ? one : many);

// ---------------------------------------------------------------------------
// File view

// parseFileHash parses "#/file/<owner>/<name>/<path>[:<line>]".
function parseFileHash(hash) {
  let rest = hash.slice('#/file/'.length);
  let line = 0;
  const m = rest.match(/:(\d+)$/);
  if (m) {
    line = Number(m[1]);
    rest = rest.slice(0, -m[0].length);
  }
  const segs = rest.split('/').map(decodeURIComponent);
  if (segs.length < 3 || !segs[0] || !segs[1]) return null;
  return { repo: `${segs[0]}/${segs[1]}`, path: segs.slice(2).join('/'), line };
}

async function showFile(loc) {
  // Abort any in-flight fetch so a slow stale response can never render
  // over a newer navigation (mirrors the search path).
  if (fileCtl) fileCtl.abort();
  fileCtl = new AbortController();
  currentFileData = null;
  fileEl.replaceChildren(fileHeader(loc, null), note('Loading…'));
  let data;
  try {
    data = await api(
      `/api/file?repo=${encodeURIComponent(loc.repo)}&path=${encodeURIComponent(loc.path)}`,
      fileCtl.signal,
    );
  } catch (e) {
    if (e.name === 'AbortError') return;
    fileEl.replaceChildren(fileHeader(loc, null), errorBox(friendlyFileError(e)));
    return;
  }
  currentFileData = data;
  const body = document.createElement('div');
  body.className = 'file-body';
  if (data.highlighted) {
    // Chroma output: all source text escaped by construction.
    body.innerHTML = data.highlighted;
  } else {
    body.append(plainPre(data.content));
  }
  fileEl.replaceChildren(fileHeader(loc, data), body);
  if (loc.line) targetLine(loc.line, true);
}

function fileHeader(loc, data) {
  const bar = document.createElement('div');
  bar.className = 'file-head';
  const back = document.createElement('a');
  back.className = 'back';
  back.href = '#/';
  back.textContent = '\u2190 results';
  back.title = 'Back to results (Esc)';
  const name = document.createElement('span');
  name.className = 'file-name';
  name.textContent = `${loc.repo}/${loc.path}`;
  bar.append(back, name);
  if (data) {
    bar.append(fileActions(loc, data));
    const meta = document.createElement('span');
    meta.className = 'file-meta';
    meta.textContent =
      `@ ${data.indexedCommit.slice(0, 7)} · ` +
      `${data.totalLines} ${plural(data.totalLines, 'line', 'lines')}`;
    bar.append(meta);
  }
  return bar;
}

const EDITOR_LABELS = { cursor: 'Cursor', vscode: 'VS Code' };

function githubUrl(loc, data) {
  const p = loc.path.split('/').map(encodeURIComponent).join('/');
  const anchor = loc.line ? `#L${loc.line}` : '';
  return `https://github.com/${loc.repo}/blob/${data.indexedCommit}/${p}${anchor}`;
}

function editorUrl(loc, data) {
  const p = data.localPath.split('/').map(encodeURIComponent).join('/');
  return `${data.editorScheme}://file${p}:${loc.line || 1}`;
}

// fileActions builds the compact header action group: copy path, GitHub
// permalink (link + copy), and open-in-editor when a local checkout exists.
function fileActions(loc, data) {
  const acts = document.createElement('span');
  acts.className = 'file-actions';
  acts.append(
    copyBtn(() => {
      const f = currentFile || loc;
      return `${f.repo}/${f.path}` + (f.line ? `:L${f.line}` : '');
    }, 'Copy path'),
  );
  const gh = document.createElement('a');
  gh.className = 'act gh';
  gh.href = githubUrl(loc, data);
  gh.target = '_blank';
  gh.rel = 'noopener';
  gh.textContent = 'GitHub';
  acts.append(gh, copyBtn(() => gh.href, 'Copy GitHub permalink', 'link'));
  if (data.localPath) {
    const ed = document.createElement('a');
    ed.className = 'act editor';
    ed.href = editorUrl(loc, data);
    ed.textContent = EDITOR_LABELS[data.editorScheme] || data.editorScheme;
    ed.title = 'Opens your local checkout — may be on a different commit than the index';
    acts.append(ed);
  }
  return acts;
}

// refreshFileActions re-points the header links at the active target line.
function refreshFileActions() {
  if (!currentFile || !currentFileData) return;
  const gh = $('.file-actions .gh', fileEl);
  if (gh) gh.href = githubUrl(currentFile, currentFileData);
  const ed = $('.file-actions .editor', fileEl);
  if (ed) ed.href = editorUrl(currentFile, currentFileData);
}

// plainPre renders unhighlighted content in the same DOM shape chroma
// emits (pre.chroma > code > span.line > span.ln + span.cl) so the gutter
// CSS, anchors, and scroll-to-line work identically.
function plainPre(content) {
  const pre = document.createElement('pre');
  pre.className = 'chroma';
  const code = document.createElement('code');
  const lines = content.split('\n');
  if (lines[lines.length - 1] === '') lines.pop();
  lines.forEach((text, i) => {
    const line = document.createElement('span');
    line.className = 'line';
    const ln = document.createElement('span');
    ln.className = 'ln';
    ln.id = 'L' + (i + 1);
    const a = document.createElement('a');
    a.className = 'lnlinks';
    a.href = '#L' + (i + 1);
    a.textContent = i + 1;
    ln.append(a);
    const cl = document.createElement('span');
    cl.className = 'cl';
    cl.textContent = text + '\n';
    line.append(ln, cl);
    code.append(line);
  });
  pre.append(code);
  return pre;
}

function targetLine(n, scroll) {
  $('.line.hl', fileEl)?.classList.remove('hl');
  const ln = document.getElementById('L' + n);
  if (!ln) return;
  const line = ln.closest('.line');
  line.classList.add('hl');
  if (scroll) line.scrollIntoView({ block: 'center' });
}

function friendlyFileError(e) {
  switch (e.status) {
    case 404:
      return `Not found at the indexed commit.\n${e.message}`;
    case 409:
      return `Index and mirror are out of sync — run muninn sync.\n${e.message}`;
    case 413:
      return `File is too large to display.\n${e.message}`;
    case 415:
      return `Binary file — nothing to display.\n${e.message}`;
    default:
      return e.message;
  }
}

// Clicking a gutter line number selects that line and updates the URL
// (shareable deep link) without adding a history entry or re-rendering.
fileEl.addEventListener('click', (e) => {
  const a = e.target.closest('a.lnlinks');
  if (!a || !currentFile) return;
  e.preventDefault();
  const n = Number(a.parentElement.id.slice(1));
  currentFile.line = n;
  history.replaceState(
    null,
    '',
    location.pathname + location.search + fileHash(currentFile.repo, currentFile.path, n),
  );
  targetLine(n, false);
  refreshFileActions();
});

// ---------------------------------------------------------------------------
// Routing (hash-based; the query lives in ?q= so searches are shareable)

function route() {
  const hash = location.hash;
  if (hash.startsWith('#/file/')) {
    const loc = parseFileHash(hash);
    if (loc) {
      if (
        currentFile &&
        currentFile.repo === loc.repo &&
        currentFile.path === loc.path &&
        document.body.dataset.view === 'file'
      ) {
        currentFile = loc;
        if (loc.line) targetLine(loc.line, true);
        refreshFileActions();
        return;
      }
      if (document.body.dataset.view === 'results') savedScroll = window.scrollY;
      currentFile = loc;
      document.body.dataset.view = 'file';
      showFile(loc);
      return;
    }
  }
  currentFile = null;
  currentFileData = null;
  if (fileCtl) fileCtl.abort();
  document.body.dataset.view = 'results';
  fileEl.replaceChildren();
  requestAnimationFrame(() => window.scrollTo(0, savedScroll));
}

// ---------------------------------------------------------------------------
// Header: syntax hint chips + repo badge

const CHIPS = [
  ['repo:', 'limit to repos matching a regex'],
  ['file:', 'limit to file paths matching a regex'],
  ['sym:', 'match symbol definitions'],
  ['lang:', 'limit to a language'],
  ['case:yes', 'case-sensitive matching'],
  ['-', 'negate the next atom'],
  ['"..."', 'exact phrase'],
];

for (const [atom, title] of CHIPS) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'chip';
  b.textContent = atom;
  b.title = title;
  b.addEventListener('click', () => insertAtom(atom));
  hintsEl.append(b);
}

function insertAtom(atom) {
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

async function loadRepos() {
  let repos;
  try {
    repos = await api('/api/repos');
  } catch {
    return; // badge is informational; leave it empty on failure
  }
  const n = repos.length;
  const age = repos[0]?.indexAge;
  repoBadge.textContent =
    `${n} ${plural(n, 'repo', 'repos')}` +
    (age && age !== 'unknown' ? ` · indexed ${age} ago` : '');
  if (repos.some((r) => r.stale)) {
    const w = document.createElement('span');
    w.className = 'stale';
    w.title = 'Index older than 24h (or never synced) — run muninn sync';
    w.textContent = 'stale';
    repoBadge.append(' ', w);
  }
}

// ---------------------------------------------------------------------------
// Helpers + input wiring

function note(text) {
  const div = document.createElement('div');
  div.className = 'note';
  div.textContent = text;
  return div;
}

function errorBox(msg) {
  const div = document.createElement('div');
  div.className = 'error';
  div.textContent = msg;
  return div;
}

searchInput.addEventListener('input', () => {
  clearTimeout(debounceTimer);
  debounceTimer = setTimeout(() => runSearch(searchInput.value.trim(), true), DEBOUNCE_MS);
});

searchInput.addEventListener('keydown', (e) => {
  if (e.key === 'Enter') {
    clearTimeout(debounceTimer);
    runSearch(searchInput.value.trim(), true);
  }
});

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && document.body.dataset.view === 'file') {
    // preventDefault: a focused type=search input would otherwise clear
    // itself natively, wiping the query on the way back to results.
    e.preventDefault();
    location.hash = '#/';
  } else if (e.key === '/' && document.activeElement !== searchInput) {
    e.preventDefault();
    searchInput.focus();
    searchInput.select();
  }
});

// ---------------------------------------------------------------------------
// Init

const initialQ = new URLSearchParams(location.search).get('q') || '';
searchInput.value = initialQ;
loadRepos();
if (initialQ) runSearch(initialQ);
else resultsEl.replaceChildren(note('Type to search across all indexed repos.'));
route();
window.addEventListener('hashchange', route);
