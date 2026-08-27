'use strict';

import { $, note } from './dom.js';
import {
  getCurrentFile,
  initFile,
  refreshFileActions,
  resetFile,
  setCurrentFile,
  showFile,
  targetLine,
} from './file.js';
import { initHints } from './query.js';
import { loadRepos } from './repos.js';
import { parseFileHash } from './routes.js';
import { initSearch, runSearch } from './search.js';

const searchInput = $('#q');
const hintsEl = $('#hints');
const resultsEl = $('#results');
const fileEl = $('#file');
const statsEl = $('#stats');
const repoBadge = $('#repo-badge');

const DEBOUNCE_MS = 200;

let debounceTimer = 0;
let savedScroll = 0; // results scroll position, restored on return

// ---------------------------------------------------------------------------
// Routing (hash-based; the query lives in ?q= so searches are shareable)

function route() {
  const hash = location.hash;
  if (hash.startsWith('#/file/')) {
    const loc = parseFileHash(hash);
    if (loc) {
      const current = getCurrentFile();
      if (
        current &&
        current.repo === loc.repo &&
        current.path === loc.path &&
        document.body.dataset.view === 'file'
      ) {
        setCurrentFile(loc);
        if (loc.line) targetLine(loc.line, true);
        refreshFileActions();
        return;
      }
      if (document.body.dataset.view === 'results') savedScroll = window.scrollY;
      setCurrentFile(loc);
      document.body.dataset.view = 'file';
      showFile(loc);
      return;
    }
  }
  resetFile();
  document.body.dataset.view = 'results';
  requestAnimationFrame(() => window.scrollTo(0, savedScroll));
}

// ---------------------------------------------------------------------------
// Input wiring

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

initSearch({ resultsEl, statsEl });
initFile(fileEl);
initHints(hintsEl, searchInput);

const initialQ = new URLSearchParams(location.search).get('q') || '';
searchInput.value = initialQ;
loadRepos(repoBadge);
if (initialQ) runSearch(initialQ);
else resultsEl.replaceChildren(note('Type to search across all indexed repos.'));
route();
window.addEventListener('hashchange', route);
