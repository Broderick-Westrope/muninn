'use strict';

import { $, copyText, isMac, note } from './dom.js';
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
  fileLineCount,
  getCurrentFile,
  initFile,
  refreshFileActions,
  resetFile,
  setCurrentFile,
  showFile,
  targetLines,
} from './file.js';
import {
  initPalette,
  openPalette,
  paletteOpen,
  rememberFile,
  togglePalette,
} from './palette.js';
import { isGoToLineKey, isPaletteKey } from './palette-match.js';
import { initHints } from './query.js';
import { loadRepos } from './repos.js';
import { parseFileHash } from './routes.js';
import { emptyPrompt, initSearch, runSearch } from './search.js';
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
        // A target-less hash (back to the bare file) clears the highlight,
        // so it never disagrees with what Copy path reports.
        targetLines(loc.line, loc.endLine, Boolean(loc.line));
        refreshFileActions();
        showTree(loc);
        return;
      }
      if (document.body.dataset.view === 'results') savedScroll = window.scrollY;
      setCurrentFile(loc);
      document.body.dataset.view = 'file';
      // Only a file that actually loaded is worth offering again.
      showFile(loc).then((ok) => {
        if (ok) rememberFile(loc);
      });
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

const inFileView = () => document.body.dataset.view === 'file';
const inResultsView = () => document.body.dataset.view === 'results';
const mod = isMac ? '⌘' : 'Ctrl';

// shortcuts are the page's global keys, checked in order. available() is
// the view condition alone; commands() lists a command with a key only when
// its shortcut is available, so the palette never advertises a dead key.
// Only the palette chord works while the palette is open: the palette stops
// its own keys from bubbling, and this guards against one reaching the page
// another way.
const shortcuts = {
  palette: {
    keys: [mod, 'K'],
    whilePaletteOpen: true,
    match: (e) => isPaletteKey(e, isMac),
    available: () => true,
    run: togglePalette,
  },
  // Only in the file view: elsewhere there is no line to go to, and the
  // browser's own find-next keeps working.
  goToLine: {
    keys: [mod, 'G'],
    match: (e) => isGoToLineKey(e, isMac),
    available: inFileView,
    run: () => openPalette(':'),
  },
  // Escape's preventDefault matters: a focused type=search input would
  // otherwise clear itself natively, wiping the query on the way back.
  back: {
    keys: ['esc'],
    match: (e) => e.key === 'Escape',
    available: inFileView,
    run: () => {
      location.hash = '#/';
    },
  },
  focusSearch: {
    keys: ['/'],
    match: (e) => e.key === '/' && document.activeElement !== searchInput,
    available: () => true,
    run: focusSearch,
  },
  // Shift-modified so it still works with the search input focused, where
  // a bare Backspace has to keep editing the query. Results view only:
  // from the file view it would act on a sidebar the user cannot see.
  clearFilters: {
    keys: ['shift', '⌫'],
    match: (e) => e.key === 'Backspace' && e.shiftKey,
    available: () => inResultsView() && activeCount() > 0,
    run: clearFacets,
  },
};

document.addEventListener('keydown', (e) => {
  for (const s of Object.values(shortcuts)) {
    if (!s.whilePaletteOpen && paletteOpen()) continue;
    if (s.match(e) && s.available()) {
      e.preventDefault();
      s.run();
      return;
    }
  }
});

const fileInView = () => (inFileView() ? getCurrentFile() : null);

// commands is the palette's command list for the current view; it is
// rebuilt on every keystroke so it always reflects the live state.
function commands() {
  const f = fileInView();
  const cmds = [{ title: 'Focus search', keys: shortcuts.focusSearch.keys, run: focusSearch }];
  if (f) {
    cmds.push(...fileCommands());
    cmds.push(
      { title: 'Go to line…', keys: shortcuts.goToLine.keys, prefix: ':' },
      { title: 'Search in this repo', detail: f.repo, run: () => searchInRepo(f.repo) },
      { title: 'Back to results', keys: shortcuts.back.keys, run: shortcuts.back.run },
    );
  }
  // "@" is a palette prefix, not a page shortcut, so it is shown as detail.
  cmds.push({ title: 'Go to symbol…', detail: 'or type @', prefix: '@' });
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
  if (shortcuts.clearFilters.available()) {
    cmds.push({ title: 'Clear all filters', keys: shortcuts.clearFilters.keys, run: clearFacets });
  }
  cmds.push({
    title: 'Copy link to this page',
    toast: 'Link copied',
    run: () => copyText(location.href),
  });
  return cmds;
}

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
  currentFile: fileInView,
  fileLines: fileLineCount,
  search: (q) => {
    // Show results at once: runSearch only leaves the file view after a
    // success, so a failure would otherwise render into the hidden pane.
    location.hash = '#/';
    searchInput.value = q;
    search(true);
  },
  searchRepo: searchInRepo,
});
// "⌘K" on a Mac, "Ctrl+K" elsewhere, the same everywhere it is shown.
const paletteChord = shortcuts.palette.keys.join(isMac ? '' : '+');
paletteBtn.textContent = paletteChord;
paletteBtn.title = `Command palette (${paletteChord})`;
paletteBtn.setAttribute('aria-label', paletteBtn.title);
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
  resultsEl.replaceChildren(note(emptyPrompt(facetParams())));
  // Renders a linked selection with no query, which fetches nothing.
  loadFacets('');
}
route();
window.addEventListener('hashchange', route);
