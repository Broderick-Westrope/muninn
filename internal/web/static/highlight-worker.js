'use strict';

// Tokenizes and renders files off the main thread for highlight.js. Messages:
//   {type: 'preload', lang}             load lang's grammar ahead of a render
//   {type: 'render', id, lang, content} reply {type: 'rendered', id, html, styles}
//                                       or {type: 'error', id, error}
//   {type: 'cancel', id}                drop a render that has not started tokenizing
// It posts {type: 'ready'} once its highlighter exists, or {type: 'failed'}
// if that fails. html is the inner <code> markup. styles lists [class, dark
// colour, light colour, fontStyle] for every token class html uses; the
// page turns them into CSS rules.

import { highlightFile } from './highlight-markup.js';
import { DAY_PALETTE, LANGS, THEME, getHighlighter } from './vendor/shiki/shiki.js';

// Create the highlighter (engine and theme) as soon as the worker starts,
// so the first render only waits for its grammar. The page only trusts the
// worker once this has succeeded.
getHighlighter().then(
  () => self.postMessage({ type: 'ready' }),
  (e) => self.postMessage({ type: 'failed', error: String(e?.stack || e) }),
);

const grammarLoads = new Map();

// loadGrammar resolves to the highlighter with lang's grammar loaded,
// sharing one in-flight load per language so a preload and a render never
// fetch or register the same grammar twice.
function loadGrammar(lang) {
  let load = grammarLoads.get(lang);
  if (!load) {
    const grammar = Object.hasOwn(LANGS, lang) ? LANGS[lang] : null;
    if (!grammar) return Promise.reject(new Error(`no vendored grammar for ${lang}`));
    load = Promise.all([getHighlighter(), grammar()]).then(async ([hl, mod]) => {
      await hl.loadLanguage(mod);
      return hl;
    });
    // A failed load is retried on the next file rather than cached.
    load.catch(() => grammarLoads.delete(lang));
    grammarLoads.set(lang, load);
  }
  return load;
}

// Renders received but not yet finished, and those of them the page has
// cancelled. Tracking only live ids keeps a late cancel from leaking.
const live = new Set();
const cancelled = new Set();

// Yield one task so a cancel the page sent after this render is handled
// before tokenizing starts; once it starts it runs to completion.
const nextTask = () => new Promise((resolve) => setTimeout(resolve, 0));

self.onmessage = async ({ data }) => {
  switch (data.type) {
    case 'preload':
      loadGrammar(data.lang).catch(() => {});
      return;
    case 'cancel':
      if (live.has(data.id)) cancelled.add(data.id);
      return;
    case 'render':
      break;
    default:
      console.warn('highlight worker: unknown message', data.type);
      return;
  }
  live.add(data.id);
  try {
    const hl = await loadGrammar(data.lang);
    await nextTask();
    if (cancelled.has(data.id)) return;
    const out = highlightFile(hl, { lang: data.lang, theme: THEME, dayPalette: DAY_PALETTE }, data.content);
    self.postMessage({ type: 'rendered', id: data.id, ...out });
  } catch (e) {
    self.postMessage({ type: 'error', id: data.id, error: String(e?.stack || e) });
  } finally {
    live.delete(data.id);
    cancelled.delete(data.id);
  }
};
