'use strict';

import { $, note } from './dom.js';
import {
  activeCount,
  clearFacets,
  facetParams,
  initFacets,
  loadFacets,
  selectOnly,
} from './facets.js';
import {
  fileCommands,
  getCurrentFile,
  initFile,
  refreshFileActions,
  resetFile,
  setCurrentFile,
  showFile,
  targetLines,
} from './file.js';
import { initPalette, isMac, paletteOpen, rememberFile, togglePalette } from './palette.js';
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
const paletteBtn = $('#palette-btn');

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
        if (loc.line) targetLines(loc.line, loc.endLine, true);
        refreshFileActions();
        showTree(loc);
        return;
      }
      if (document.body.dataset.view === 'results') savedScroll = window.scrollY;
      setCurrentFile(loc);
      document.body.dataset.view = 'file';
      rememberFile(loc);
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
//
// Results and facets are fired independently and never await each other:
// aggregation is exhaustive and can take far longer than the capped results
// pass, so coupling them would make every keystroke wait on it.
function search(fromUser, facetsImmediate) {
  const q = searchInput.value.trim();
  runSearch(q, fromUser, facetParams());
  loadFacets(q, facetsImmediate);
}

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

// ---------------------------------------------------------------------------
// Command palette

function focusSearch() {
  searchInput.focus();
  searchInput.select();
}

// searchInRepo narrows results to one repo and returns to them, ready for a
// query.
function searchInRepo(repo) {
  selectOnly('repo', repo);
  location.hash = '#/';
  focusSearch();
}

// commands is the palette's command list for the current view; it is
// rebuilt on every keystroke so it always reflects the live state.
function commands() {
  const inFile = document.body.dataset.view === 'file';
  const f = getCurrentFile();
  const cmds = [{ title: 'Focus search', keys: ['/'], run: focusSearch }];
  if (inFile && f) {
    cmds.push(...fileCommands());
    cmds.push(
      { title: 'Go to line…', keys: [':'], stay: ':' },
      { title: 'Search in this repo', detail: f.repo, run: () => searchInRepo(f.repo) },
      {
        title: 'Back to results',
        keys: ['esc'],
        run: () => {
          location.hash = '#/';
        },
      },
    );
  }
  cmds.push({ title: 'Go to symbol…', keys: ['@'], stay: '@' });
  if (searchInput.value) {
    cmds.push({
      title: 'Clear search',
      run: () => {
        searchInput.value = '';
        search(true);
        focusSearch();
      },
    });
  }
  if (activeCount() > 0) {
    cmds.push({ title: 'Clear all filters', keys: ['shift', '⌫'], run: clearFacets });
  }
  cmds.push({
    title: 'Copy link to this page',
    toast: 'Link copied',
    run: () => navigator.clipboard.writeText(location.href),
  });
  return cmds;
}

document.addEventListener('keydown', (e) => {
  if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'k') {
    e.preventDefault();
    togglePalette();
    return;
  }
  // The palette stops its own keys from bubbling; this guards against a
  // key that reaches the page while it is open some other way.
  if (paletteOpen()) return;
  if (e.key === 'Escape' && document.body.dataset.view === 'file') {
    // preventDefault: a focused type=search input would otherwise clear
    // itself natively, wiping the query on the way back to results.
    e.preventDefault();
    location.hash = '#/';
  } else if (e.key === '/' && document.activeElement !== searchInput) {
    e.preventDefault();
    searchInput.focus();
    searchInput.select();
  } else if (e.key === 'Backspace' && e.shiftKey) {
    // Shift-modified so it still works with the search input focused,
    // where a bare Backspace has to keep editing the query. Esc is already
    // taken by the file view, and clearing filters from there would act on
    // a sidebar the user cannot see.
    if (document.body.dataset.view !== 'results') return;
    e.preventDefault();
    clearFacets();
  }
});

// ---------------------------------------------------------------------------
// Init

initSearch({ resultsEl, statsEl });
initFile(fileEl);
initTree(treeEl);
// A facet toggle re-searches immediately: it is a click, not typing, so it
// must not wait out the input debounce. The universe does not depend on the
// selection, so its refetch is immediate and cheap (same query).
initFacets({ facetsEl, onToggle: () => search(false, true) });
initHints(hintsEl, searchInput);
initPalette({
  commands,
  currentFile: () => (document.body.dataset.view === 'file' ? getCurrentFile() : null),
  search: (q) => {
    searchInput.value = q;
    search(true);
  },
  searchRepo: searchInRepo,
});
paletteBtn.textContent = isMac ? '⌘K' : 'Ctrl K';
paletteBtn.title = `Command palette (${isMac ? '⌘K' : 'Ctrl+K'})`;
paletteBtn.addEventListener('click', togglePalette);

const initialQ = new URLSearchParams(location.search).get('q') || '';
searchInput.value = initialQ;
loadRepos(repoBadge);
// initFacets has already read the URL, so a reloaded filtered link searches
// with its facets applied rather than briefly showing everything.
if (initialQ) {
  runSearch(initialQ, false, facetParams());
  loadFacets(initialQ, true);
} else {
  resultsEl.replaceChildren(note('Type to search across all indexed repos.'));
}
route();
window.addEventListener('hashchange', route);
