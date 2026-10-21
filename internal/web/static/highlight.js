'use strict';

import { TOKEN_CLASS, TOKEN_COLOR } from './highlight-markup.js';
import { languageFor } from './languages.js';

// The worker tokenizes off the main thread so large files never freeze the
// page. It is created on first use. If it fails before it is ready (its
// module could not load, its highlighter could not start, or module workers
// are unsupported) highlighting stays off for the page's lifetime and the
// server renders with chroma; a later crash or hang just restarts it.
let worker = null;
let workerReady = false;
let workerBroken = false;
let nextId = 0;
const pending = new Map();

// Cancelling a render this large terminates the worker instead of letting
// a tokenize nobody will see hold up the next file. Smaller renders finish
// sooner than a fresh worker could warm up.
const RESTART_LINES = 2000;
const RESTART_CHARS = 256 * 1024;

// A render unanswered for this long is treated as a hung worker: well past
// the seconds a 10,000-line file takes, short enough that the next file is
// not stuck behind it for long.
const RENDER_TIMEOUT_MS = 15_000;

// Shiki FontStyle bit flags.
const ITALIC = 1;
const BOLD = 2;
const UNDERLINE = 4;
const STRIKETHROUGH = 8;

let styleEl = null;
const styledClasses = new Set();

function fontDecls(fontStyle) {
  const out = [];
  if (fontStyle & ITALIC) out.push('font-style:italic');
  if (fontStyle & BOLD) out.push('font-weight:bold');
  const lines = [];
  if (fontStyle & UNDERLINE) lines.push('underline');
  if (fontStyle & STRIKETHROUGH) lines.push('line-through');
  if (lines.length) out.push(`text-decoration:${lines.join(' ')}`);
  return out.map((d) => `;${d}`).join('');
}

// addStyles inserts CSS for token classes the page has not seen yet. Class
// names encode the style, so a class already inserted never needs updating.
// Values are checked before they reach the stylesheet: they come from the
// build-time theme today, but a bad one must not inject CSS.
function addStyles(styles) {
  if (!styleEl) {
    styleEl = document.createElement('style');
    document.head.append(styleEl);
  }
  const sheet = styleEl.sheet;
  for (const [cls, dark, light, fontStyle] of styles) {
    if (styledClasses.has(cls)) continue;
    if (!TOKEN_CLASS.test(cls) || !TOKEN_COLOR.test(dark) || !TOKEN_COLOR.test(light)) {
      throw new Error(`invalid token style ${JSON.stringify([cls, dark, light])}`);
    }
    // Light by default with a dark override, as in style.css.
    const font = fontDecls(Number(fontStyle) | 0);
    sheet.insertRule(`.shiki .${cls} { color:${light}${font} }`, sheet.cssRules.length);
    sheet.insertRule(
      `@media (prefers-color-scheme: dark) { .shiki .${cls} { color:${dark} } }`,
      sheet.cssRules.length,
    );
    styledClasses.add(cls);
  }
}

function settle(id, html) {
  const job = pending.get(id);
  if (!job) return;
  pending.delete(id);
  job.resolve(html);
}

function getWorker() {
  if (worker || workerBroken) return worker;
  let w;
  try {
    w = new Worker(new URL('./highlight-worker.js', import.meta.url), { type: 'module' });
  } catch (e) {
    console.warn('shiki worker unavailable; using server highlighting', e);
    workerBroken = true;
    return null;
  }
  worker = w;
  workerReady = false;
  // Events from a worker that has since been replaced are ignored, so a
  // stopped worker can never mark its successor ready or terminate it.
  const handlers = {
    ready: () => {
      workerReady = true;
    },
    failed: (data) => {
      console.warn('shiki highlighter failed to start; using server highlighting', data.error);
      workerBroken = true;
      stopWorker();
    },
    error: (data) => {
      console.warn('shiki highlighting failed', data.error);
      settle(data.id, null);
    },
    rendered: (data) => {
      // A reply for a render that was cancelled or timed out is dropped
      // before its styles reach the sheet.
      if (!pending.has(data.id)) return;
      try {
        addStyles(data.styles);
      } catch (e) {
        console.warn('shiki highlighting failed', e);
        settle(data.id, null);
        return;
      }
      settle(data.id, data.html);
    },
  };
  w.onmessage = ({ data }) => {
    if (w !== worker) return;
    const handle = Object.hasOwn(handlers, data?.type) ? handlers[data.type] : null;
    if (handle) handle(data);
    else console.warn('highlight worker: unknown message', data?.type);
  };
  w.onerror = (e) => {
    e.preventDefault();
    if (w !== worker) return;
    failWorker(`shiki worker failed: ${e.message ?? e.type}`);
  };
  return w;
}

// failWorker stops a worker that crashed or hung. One that never became
// ready (including a restarted one) is taken to be unable to run here, for
// example because its module or highlighter fails to load, so the server
// highlights from now on rather than retrying every file. One that was
// working is restarted on the next render.
function failWorker(reason) {
  if (!workerReady) {
    console.warn(`${reason} before starting; using server highlighting`);
    workerBroken = true;
  } else {
    console.warn(`${reason}; restarting the worker`);
  }
  stopWorker();
}

// stopWorker terminates the worker and resolves everything it owed with
// null; the next render starts a fresh one.
function stopWorker() {
  worker?.terminate();
  worker = null;
  workerReady = false;
  for (const id of [...pending.keys()]) settle(id, null);
}

function cancel(id) {
  const job = pending.get(id);
  if (!job) return;
  if (job.large) {
    stopWorker();
    return;
  }
  settle(id, null);
  worker?.postMessage({ type: 'cancel', id });
}

// shikiAvailable reports whether the page will highlight path itself, so
// the caller can tell the server to skip chroma (highlight=0).
export function shikiAvailable(path) {
  return !workerBroken && languageFor(path) !== null;
}

// shikiWanted reports whether file, an /api/file response for path, is one
// Shiki should colour: a vendored language, within the server's
// highlighting caps, and not empty.
function shikiWanted(path, file) {
  return languageFor(path) !== null && file.highlightable && file.content !== '';
}

// warmHighlighter starts the worker while the browser is idle, so opening
// the first file only waits for its grammar and first tokenize.
export function warmHighlighter() {
  if ('requestIdleCallback' in window) requestIdleCallback(getWorker);
  else setTimeout(getWorker, 0);
}

// preloadGrammar starts loading path's grammar, if Shiki has one, so it
// downloads while the file itself is being fetched. It never throws.
export function preloadGrammar(path) {
  const lang = languageFor(path);
  if (!lang) return;
  try {
    getWorker()?.postMessage({ type: 'preload', lang });
  } catch (e) {
    console.warn('shiki preload failed', e);
  }
}

const SKIPPED = Object.freeze({ pre: null, failed: false });
const FAILED = Object.freeze({ pre: null, failed: true });

// highlightPre renders file, an /api/file response for path, as a pre in
// the same DOM shape as plainPre in file.js, so the gutter CSS, anchors and
// range selection keep working. It never rejects. It resolves to {pre} on
// success; to {pre: null, failed: true} when Shiki should have coloured the
// file but couldn't (the worker is unusable, crashed, hung or reported an
// error), so the caller should ask the server for chroma; and to
// {pre: null, failed: false} when Shiki has nothing to do or signal aborts.
// An abort also stops the worker's pending work for it.
export async function highlightPre(path, file, signal) {
  if (!shikiWanted(path, file) || signal.aborted) return SKIPPED;
  const lang = languageFor(path);
  const { content, totalLines } = file;
  try {
    const w = getWorker();
    if (!w) return FAILED;
    const id = nextId++;
    const onAbort = () => cancel(id);
    signal.addEventListener('abort', onAbort, { once: true });
    let html;
    let timer;
    try {
      html = await new Promise((resolve) => {
        pending.set(id, {
          resolve,
          large: totalLines > RESTART_LINES || content.length > RESTART_CHARS,
        });
        timer = setTimeout(() => failWorker('shiki render timed out'), RENDER_TIMEOUT_MS);
        w.postMessage({ type: 'render', id, lang, content });
      });
    } finally {
      clearTimeout(timer);
      signal.removeEventListener('abort', onAbort);
      pending.delete(id);
    }
    if (signal.aborted) return SKIPPED;
    if (html === null) return FAILED;
    const pre = document.createElement('pre');
    pre.className = 'chroma shiki';
    // Worker output: all source text escaped by highlightFile in
    // highlight-markup.js.
    pre.innerHTML = `<code>${html}</code>`;
    return { pre, failed: false };
  } catch (e) {
    console.warn('shiki highlighting failed', e);
    return signal.aborted ? SKIPPED : FAILED;
  }
}
