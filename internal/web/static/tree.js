'use strict';

import { api } from './api.js';
import { errorBox } from './dom.js';
import { fileHash } from './routes.js';

let treeEl = null;
let treeRepo = null; // repo the rendered tree belongs to
let treeCtl = null; // aborted on repo change only, never on same-repo nav
let currentPath = null; // file to mark as current
// dir path -> {entries, truncated} | 'loading' | {error}
let nodes = new Map();

export function initTree(el) {
  treeEl = el;
  // Directory toggles are buttons inside the tree; one delegated listener
  // survives every re-render.
  treeEl.addEventListener('click', (e) => {
    const b = e.target.closest('button.tree-dir');
    if (b) {
      toggleDir(b.dataset.path);
      return;
    }
    const retry = e.target.closest('button.tree-retry');
    if (retry) loadDir(retry.dataset.path, true);
  });
}

// showTree renders the tree for a file location, expanding every ancestor
// of the file. Navigating within a repo keeps expansion state (and any
// in-flight expand); changing repo resets it.
export function showTree(loc) {
  if (loc.repo !== treeRepo) {
    if (treeCtl) treeCtl.abort();
    treeCtl = new AbortController();
    treeRepo = loc.repo;
    nodes = new Map();
  }
  currentPath = loc.path;

  // Ancestor spine: "" (root), then each directory containing the file.
  const segs = loc.path.split('/').slice(0, -1);
  const spine = [''];
  for (let i = 0; i < segs.length; i++) spine.push(segs.slice(0, i + 1).join('/'));

  render();
  for (const dir of spine) {
    if (!nodes.has(dir)) loadDir(dir);
  }
}

async function loadDir(dir, force) {
  if (!force && nodes.get(dir) === 'loading') return;
  const repo = treeRepo;
  const signal = treeCtl.signal;
  nodes.set(dir, 'loading');
  render();
  let data;
  try {
    data = await api(
      `/api/tree?repo=${encodeURIComponent(repo)}&path=${encodeURIComponent(dir)}`,
      signal,
    );
  } catch (e) {
    if (e.name === 'AbortError') return;
    if (!isCurrent(repo)) return;
    nodes.set(dir, { error: friendlyTreeError(e) });
    render();
    return;
  }
  // The repo check alone would not cover pressing Esc back to results: a
  // slow response would then paint the tree over the facet panel.
  if (!isCurrent(repo)) return;
  nodes.set(dir, { entries: data.entries, truncated: data.truncated });
  render();
}

const isCurrent = (repo) => repo === treeRepo && document.body.dataset.view === 'file';

function toggleDir(dir) {
  if (nodes.has(dir)) nodes.delete(dir);
  else loadDir(dir);
  render();
}

function friendlyTreeError(e) {
  switch (e.status) {
    case 404:
      return 'Not found at the indexed commit';
    case 409:
      return 'Index and mirror are out of sync — run muninn sync';
    default:
      return e.message;
  }
}

// ---------------------------------------------------------------------------
// Rendering

function render() {
  if (!treeRepo) {
    treeEl.replaceChildren();
    return;
  }
  const head = document.createElement('div');
  head.className = 'tree-head';
  head.textContent = treeRepo.split('/').slice(1).join('/') || treeRepo;
  head.title = treeRepo;
  treeEl.replaceChildren(head, dirList(''));
}

// dirList renders one directory's children, recursing into expanded ones.
function dirList(dir) {
  const ul = document.createElement('ul');
  ul.className = 'tree-list';
  const node = nodes.get(dir);

  if (node === 'loading') {
    ul.append(leafNote('Loading…', 'tree-loading'));
    return ul;
  }
  if (node && node.error) {
    const li = document.createElement('li');
    li.className = 'tree-error';
    li.append(errorBox(node.error));
    const retry = document.createElement('button');
    retry.type = 'button';
    retry.className = 'tree-retry';
    retry.dataset.path = dir;
    retry.textContent = 'Retry';
    li.append(retry);
    ul.append(li);
    return ul;
  }
  if (!node) return ul;

  // Directories first, then files, each alphabetical.
  const entries = [...node.entries].sort((a, b) => {
    if (a.type !== b.type) return a.type === 'dir' ? -1 : 1;
    return a.path.localeCompare(b.path);
  });
  for (const e of entries) ul.append(e.type === 'dir' ? dirItem(e) : fileItem(e));
  if (node.truncated) ul.append(leafNote('listing truncated', 'tree-trunc'));
  return ul;
}

function dirItem(e) {
  const li = document.createElement('li');
  const open = nodes.has(e.path);
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'tree-dir' + (open ? ' open' : '');
  b.dataset.path = e.path;
  b.setAttribute('aria-expanded', open ? 'true' : 'false');
  b.textContent = basename(e.path);
  li.append(b);
  if (open) li.append(dirList(e.path));
  return li;
}

function fileItem(e) {
  const li = document.createElement('li');
  const a = document.createElement('a');
  a.className = 'tree-file' + (e.path === currentPath ? ' current' : '');
  a.href = fileHash(treeRepo, e.path);
  a.textContent = basename(e.path);
  a.title = e.path;
  if (e.path === currentPath) a.setAttribute('aria-current', 'page');
  li.append(a);
  return li;
}

function leafNote(text, className) {
  const li = document.createElement('li');
  li.className = className;
  li.textContent = text;
  return li;
}

const basename = (p) => p.slice(p.lastIndexOf('/') + 1);
