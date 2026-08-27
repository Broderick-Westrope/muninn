'use strict';

import { api } from './api.js';
import { copyBtn, errorBox, markMatches, note, plural } from './dom.js';
import { bareTokens } from './query.js';
import { fileHash } from './routes.js';

const SEARCH_LIMIT = 100;

let resultsEl = null;
let statsEl = null;
let searchCtl = null; // AbortController for the in-flight search

export function initSearch(els) {
  resultsEl = els.resultsEl;
  statsEl = els.statsEl;
}

export async function runSearch(q, fromUser) {
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
    section.append(repoHeading(repo, files));
    for (const f of files) section.append(fileCard(f, tokens));
    frag.append(section);
  }
  resultsEl.replaceChildren(frag);
  renderStats(data);
}

// repoHeading renders "owner/name" with the owner de-emphasised, so the
// distinguishing half wins the glance, plus the group's match count.
function repoHeading(repo, files) {
  const h = document.createElement('h2');
  const [owner, ...rest] = repo.split('/');
  const ownerEl = document.createElement('span');
  ownerEl.className = 'repo-owner';
  ownerEl.textContent = owner + '/';
  const nameEl = document.createElement('span');
  nameEl.className = 'repo-name';
  nameEl.textContent = rest.join('/');
  // A filename-only match carries no lines but is still one result, the
  // same rule the server applies when it counts against the limit.
  const count = files.reduce((n, f) => n + (f.lines.length || 1), 0);
  const countEl = document.createElement('span');
  countEl.className = 'repo-count';
  countEl.textContent = `${count} ${plural(count, 'match', 'matches')}`;
  h.append(ownerEl, nameEl, countEl);
  return h;
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
