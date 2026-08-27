'use strict';

import { $, note } from './dom.js';
import { facetParams, initFacets, renderFacets } from './facets.js';
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
import { initTree, showTree } from './tree.js';

const searchInput = $('#q');
const hintsEl = $('#hints');
const resultsEl = $('#results');
const fileEl = $('#file');
const statsEl = $('#stats');
const repoBadge = $('#repo-badge');
const treeEl = $('#sidebar-tree');
const facetsEl = $('#sidebar-facets');

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
        showTree(loc);
        return;
      }
      if (document.body.dataset.view === 'results') savedScroll = window.scrollY;
      setCurrentFile(loc);
      document.body.dataset.view = 'file';
      showFile(loc);
      showTree(loc);
      return;
    }
  }
  resetFile();
  document.body.dataset.view = 'results';
  requestAnimationFrame(() => window.scrollTo(0, savedScroll));
}

// ---------------------------------------------------------------------------
// Input wiring

// search takes facet params as an argument, so search.js never imports
// facets.js and facets.js never imports search.js.
const search = (fromUser) => runSearch(searchInput.value.trim(), fromUser, facetParams());

searchInput.addEventListener('input', () => {
  clearTimeout(debounceTimer);
  debounceTimer = setTimeout(() => search(true), DEBOUNCE_MS);
});

searchInput.addEventListener('keydown', (e) => {
  if (e.key === 'Enter') {
    clearTimeout(debounceTimer);
    search(true);
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

initSearch({ resultsEl, statsEl, onFacets: renderFacets });
initFile(fileEl);
initTree(treeEl);
// A facet toggle re-searches immediately: it is a click, not typing, so it
// must not wait out the input debounce.
initFacets({ facetsEl, onToggle: () => search(false) });
initHints(hintsEl, searchInput);

const initialQ = new URLSearchParams(location.search).get('q') || '';
searchInput.value = initialQ;
loadRepos(repoBadge);
// initFacets has already read the URL, so a reloaded filtered link searches
// with its facets applied rather than briefly showing everything.
if (initialQ) runSearch(initialQ, false, facetParams());
else resultsEl.replaceChildren(note('Type to search across all indexed repos.'));
route();
window.addEventListener('hashchange', route);
