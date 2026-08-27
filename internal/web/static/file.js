'use strict';

import { api } from './api.js';
import { $, copyBtn, errorBox, note, plural } from './dom.js';
import { fileHash } from './routes.js';

let fileEl = null;
let fileCtl = null; // AbortController for the in-flight file fetch
let currentFile = null; // {repo, path, line} when the file view is showing
let currentFileData = null; // /api/file response for the showing file

export function initFile(el) {
  fileEl = el;
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
export function refreshFileActions() {
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

export function targetLine(n, scroll) {
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
