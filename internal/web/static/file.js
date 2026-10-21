'use strict';

import { api } from './api.js';
import { $, copyBtn, copyText, errorBox, note, plural } from './dom.js';
import { fileHash } from './routes.js';

let fileEl = null;
let fileCtl = null; // AbortController for the in-flight file fetch
let currentFile = null; // {repo, path, line, endLine} when the file view is showing
let currentFileData = null; // /api/file response for the showing file

export function initFile(el) {
  fileEl = el;
  // Clicking a gutter line number selects that line, and shift-clicking
  // extends the selection to a range from the first selected line, as on
  // GitHub. The URL is updated (shareable deep link) without adding a
  // history entry or re-rendering.
  fileEl.addEventListener('click', (e) => {
    const a = e.target.closest('a.lnlinks');
    if (!a || !currentFile) return;
    e.preventDefault();
    const n = Number(a.parentElement.id.slice(1));
    const anchor = currentFile.line;
    if (e.shiftKey && anchor && anchor !== n) {
      currentFile.line = Math.min(anchor, n);
      currentFile.endLine = Math.max(anchor, n);
    } else {
      currentFile.line = n;
      currentFile.endLine = 0;
    }
    const { repo, path, line, endLine } = currentFile;
    history.replaceState(
      null,
      '',
      location.pathname + location.search + fileHash(repo, path, line, endLine),
    );
    targetLines(line, endLine, false);
    refreshFileActions();
  });
  // Shift-click would otherwise extend a text selection across the code.
  fileEl.addEventListener('mousedown', (e) => {
    if (e.shiftKey && e.target.closest('a.lnlinks')) e.preventDefault();
  });
}

export const getCurrentFile = () => currentFile;
export const setCurrentFile = (loc) => {
  currentFile = loc;
};

// resetFile returns the file view to its empty state: it aborts any
// in-flight fetch so a slow response can never paint over the results view,
// and clears the pane.
export function resetFile() {
  currentFile = null;
  currentFileData = null;
  if (fileCtl) fileCtl.abort();
  fileEl.replaceChildren();
}

export async function showFile(loc) {
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
  // A go-to-line, gutter click or back/forward within this file while it
  // loaded has already moved currentFile on; honour that over the location
  // the fetch started with.
  const same = currentFile && currentFile.repo === loc.repo && currentFile.path === loc.path;
  const target = same ? currentFile : loc;
  fileEl.replaceChildren(fileHeader(target, data), body);
  if (target.line) targetLines(target.line, target.endLine, true);
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

// GitHub renders these formats instead of showing source, and line anchors
// only resolve on the unrendered view (?plain=1).
const GITHUB_RENDERED = new Set([
  'md', 'markdown', 'mdown', 'mkdn', 'mkd', 'mdwn', 'mdtxt', 'mdtext', 'rmd',
  'rst', 'rest', 'adoc', 'asciidoc', 'asc', 'org', 'textile', 'rdoc',
  'creole', 'mediawiki', 'wiki', 'pod', 'pod6', 'ipynb', 'csv', 'tsv',
  'svg', 'geojson', 'topojson',
]);

function githubRendered(path) {
  const base = path.slice(path.lastIndexOf('/') + 1);
  const dot = base.lastIndexOf('.');
  return dot > 0 && GITHUB_RENDERED.has(base.slice(dot + 1).toLowerCase());
}

// lineLabel renders a target as "L10" or "L10-L20", the form both GitHub
// anchors and the copied path use.
function lineLabel(loc) {
  if (!loc.line) return '';
  return loc.endLine > loc.line ? `L${loc.line}-L${loc.endLine}` : `L${loc.line}`;
}

function githubUrl(loc, data) {
  const p = loc.path.split('/').map(encodeURIComponent).join('/');
  const label = lineLabel(loc);
  const plain = label && githubRendered(loc.path) ? '?plain=1' : '';
  const anchor = label ? `#${label}` : '';
  return `https://github.com/${loc.repo}/blob/${data.indexedCommit}/${p}${plain}${anchor}`;
}

function editorUrl(loc, data) {
  const p = data.localPath.split('/').map(encodeURIComponent).join('/');
  return `${data.editorScheme}://file${p}:${loc.line || 1}`;
}

const pathLabel = (f) => `${f.repo}/${f.path}` + (f.line ? `:${lineLabel(f)}` : '');

// fileCommands lists the showing file's actions for the command palette.
// Each reads the target when run, so a gutter selection made after the
// palette was built is still honoured. Empty when no file has loaded.
export function fileCommands() {
  const data = currentFileData;
  if (!currentFile || !data) return [];
  const cmds = [
    {
      title: 'Copy path',
      detail: pathLabel(currentFile),
      toast: 'Path copied',
      run: () => copyText(pathLabel(currentFile)),
    },
    {
      title: 'Copy GitHub permalink',
      detail: lineLabel(currentFile) || 'whole file',
      toast: 'Permalink copied',
      run: () => copyText(githubUrl(currentFile, data)),
    },
    {
      title: 'Open on GitHub',
      detail: lineLabel(currentFile) || 'whole file',
      run: () => window.open(githubUrl(currentFile, data), '_blank', 'noopener'),
    },
  ];
  if (data.localPath) {
    const label = EDITOR_LABELS[data.editorScheme] || data.editorScheme;
    cmds.push({
      title: `Open in ${label}`,
      detail: 'local checkout',
      run: () => openInEditor(currentFile, data, $('.file-actions .editor', fileEl)),
    });
  }
  return cmds;
}

// fileActions builds the compact header action group: copy path, GitHub
// permalink (link + copy), and open-in-editor when a local checkout exists.
function fileActions(loc, data) {
  const acts = document.createElement('span');
  acts.className = 'file-actions';
  acts.append(copyBtn(() => pathLabel(currentFile || loc), 'Copy path'));
  const gh = document.createElement('a');
  gh.className = 'act gh';
  gh.href = githubUrl(loc, data);
  gh.target = '_blank';
  gh.rel = 'noopener';
  gh.textContent = 'GitHub';
  gh.title = 'Shift-click line numbers to select a range';
  acts.append(gh, copyBtn(() => gh.href, 'Copy GitHub permalink', 'link'));
  if (data.localPath) acts.append(editorButton(loc, data));
  return acts;
}

// editorButton posts to /api/open, which launches the editor CLI with the
// repo's checkout as the workspace folder. The URL scheme cannot express a
// workspace folder, so it is only a fallback for when the CLI is missing.
function editorButton(loc, data) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'act editor';
  b.textContent = EDITOR_LABELS[data.editorScheme] || data.editorScheme;
  b.title = 'Open in your local checkout — may be on a different commit than the index';
  // Read the target at click time so a gutter-selected line is honoured.
  b.addEventListener('click', () => openInEditor(currentFile || loc, data, b));
  return b;
}

// openInEditor opens f in the local checkout, reporting failures next to
// btn (the header's editor button).
async function openInEditor(f, data, btn) {
  let res;
  try {
    res = await fetch('/api/open', {
      method: 'POST',
      // Sec-Fetch-Site is set by the browser; it is a forbidden header
      // name, so any attempt to set it here would be dropped anyway.
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ repo: f.repo, path: f.path, line: f.line || 1 }),
    });
  } catch (e) {
    showActionError(btn, e.message);
    return;
  }
  if (res.status === 204) return;

  // 501 is the one recoverable case: no CLI, or a path --goto cannot
  // express. The scheme opens the file without its repo, so say so.
  if (res.status === 501) {
    btn.title =
      'Opened without the repo loaded — install the editor’s shell command ' +
      'so muninn can open the workspace folder';
    location.href = editorUrl(f, data);
    return;
  }
  let msg = res.statusText;
  try {
    msg = (await res.json()).error || msg;
  } catch {
    /* non-JSON error body */
  }
  showActionError(btn, msg);
}

// showActionError surfaces an open-in-editor failure next to the button.
function showActionError(btn, msg) {
  const prev = $('.act-error', fileEl);
  if (prev) prev.remove();
  const span = document.createElement('span');
  span.className = 'act-error';
  span.textContent = msg;
  btn.after(span);
}

// refreshFileActions re-points the header links at the active target line.
// The editor button needs no update: it reads the current target when
// clicked.
export function refreshFileActions() {
  if (!currentFile || !currentFileData) return;
  const gh = $('.file-actions .gh', fileEl);
  if (gh) gh.href = githubUrl(currentFile, currentFileData);
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

// targetLines highlights line through endLine (just line when endLine is 0)
// and optionally scrolls the first highlighted line into view.
export function targetLines(line, endLine, scroll) {
  for (const el of fileEl.querySelectorAll('.line.hl')) el.classList.remove('hl');
  const last = endLine > line ? endLine : line;
  let first = null;
  for (let n = line; n <= last; n++) {
    const ln = document.getElementById('L' + n);
    if (!ln) break;
    const el = ln.closest('.line');
    el.classList.add('hl');
    first ??= el;
  }
  if (first && scroll) first.scrollIntoView({ block: last > line ? 'start' : 'center' });
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
