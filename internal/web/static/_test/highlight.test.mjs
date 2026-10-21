import assert from 'node:assert/strict';
import { beforeEach, test } from 'node:test';

// highlight.js drives a Web Worker and writes a <style> sheet. These fakes
// stand in for both so its worker lifecycle can be tested in Node; each
// test imports a fresh copy of the module so state never leaks between them.

let workers;
let sheet;

class FakeWorker {
  constructor(url, opts) {
    this.url = String(url);
    this.opts = opts;
    this.sent = [];
    this.terminated = false;
    workers.push(this);
  }
  postMessage(msg) {
    this.sent.push(msg);
  }
  terminate() {
    this.terminated = true;
  }
  // Test helpers: deliver worker-side events to the page.
  reply(data) {
    this.onmessage({ data });
  }
  crash() {
    this.onerror({ preventDefault() {}, message: 'boom' });
  }
  renders() {
    return this.sent.filter((m) => m.type === 'render');
  }
}

beforeEach(() => {
  workers = [];
  sheet = { cssRules: [], insertRule(rule, i) { this.cssRules.splice(i, 0, rule); } };
  globalThis.Worker = FakeWorker;
  globalThis.window = globalThis;
  globalThis.document = {
    head: { append() {} },
    createElement: (tag) => (tag === 'style' ? { sheet } : { className: '', innerHTML: '' }),
  };
});

let fresh = 0;
const load = () => import(`../highlight.js?t=${fresh++}`);

const file = (content = 'package x\n', totalLines = 1) => ({ content, totalLines, highlightable: true });
const tick = () => new Promise((r) => setImmediate(r));
const STYLE = ['sk-bb9af7-0', '#bb9af7', '#9854f1', 0];
// highlightPre results: Shiki had nothing to do (or was cancelled), or it
// should have coloured the file and couldn't, so the server must.
const SKIPPED = { pre: null, failed: false };
const FAILED = { pre: null, failed: true };

test('a rendered file arrives as a pre with its token styles', async () => {
  const hl = await load();
  const out = hl.highlightPre('a.go', file(), new AbortController().signal);
  const [w] = workers;
  w.reply({ type: 'ready' });
  const [msg] = w.renders();
  assert.equal(msg.lang, 'go');
  w.reply({ type: 'rendered', id: msg.id, html: '<span class="sk-bb9af7-0">package</span>', styles: [STYLE] });
  const { pre, failed } = await out;
  assert.equal(failed, false);
  assert.equal(pre.className, 'chroma shiki');
  assert.match(pre.innerHTML, /^<code><span class="sk-bb9af7-0">package<\/span><\/code>$/);
  assert.deepEqual(sheet.cssRules, [
    '.shiki .sk-bb9af7-0 { color:#9854f1 }',
    '@media (prefers-color-scheme: dark) { .shiki .sk-bb9af7-0 { color:#bb9af7 } }',
  ]);
});

test('unknown languages, files past the caps and empty files are not sent', async () => {
  const hl = await load();
  const signal = new AbortController().signal;
  assert.deepEqual(await hl.highlightPre('go.sum', file(), signal), SKIPPED);
  assert.deepEqual(await hl.highlightPre('a.go', { ...file(), highlightable: false }, signal), SKIPPED);
  assert.deepEqual(await hl.highlightPre('a.go', file('', 0), signal), SKIPPED);
  assert.equal(workers.length, 0);
});

test('an invalid token style yields null and no CSS', async () => {
  const hl = await load();
  const out = hl.highlightPre('a.go', file(), new AbortController().signal);
  const [w] = workers;
  const [msg] = w.renders();
  w.reply({ type: 'rendered', id: msg.id, html: 'x', styles: [['sk-1-0', 'red;} body{display:none', '#000000', 0]] });
  assert.deepEqual(await out, FAILED);
  assert.deepEqual(sheet.cssRules, []);
});

test('a worker that fails to start hands highlighting back to the server', async () => {
  const hl = await load();
  assert.equal(hl.shikiAvailable('a.go'), true);
  const out = hl.highlightPre('a.go', file(), new AbortController().signal);
  workers[0].reply({ type: 'failed', error: 'no engine' });
  assert.deepEqual(await out, FAILED);
  assert.equal(workers[0].terminated, true);
  assert.equal(hl.shikiAvailable('a.go'), false);
  assert.deepEqual(await hl.highlightPre('a.go', file(), new AbortController().signal), FAILED);
  assert.equal(workers.length, 1);
});

test('a worker that errors before it is ready hands highlighting back to the server', async () => {
  const hl = await load();
  const out = hl.highlightPre('a.go', file(), new AbortController().signal);
  workers[0].crash();
  assert.deepEqual(await out, FAILED);
  assert.equal(hl.shikiAvailable('a.go'), false);
});

test('a crash after start restarts the worker on the next render', async () => {
  const hl = await load();
  const first = hl.highlightPre('a.go', file(), new AbortController().signal);
  workers[0].reply({ type: 'ready' });
  workers[0].crash();
  assert.deepEqual(await first, FAILED);
  assert.equal(hl.shikiAvailable('a.go'), true);
  const ctl = new AbortController();
  const second = hl.highlightPre('a.go', file(), ctl.signal);
  assert.equal(workers.length, 2);
  assert.equal(workers[1].renders().length, 1);
  ctl.abort();
  assert.deepEqual(await second, SKIPPED);
});

test('events from a replaced worker are ignored', async () => {
  const hl = await load();
  hl.highlightPre('a.go', file(), new AbortController().signal);
  const old = workers[0];
  old.reply({ type: 'ready' });
  old.crash();
  const out = hl.highlightPre('a.go', file(), new AbortController().signal);
  const replacement = workers[1];
  old.crash();
  old.reply({ type: 'ready' });
  assert.equal(replacement.terminated, false);
  replacement.crash();
  assert.deepEqual(await out, FAILED);
  assert.equal(hl.shikiAvailable('a.go'), false, 'the stale ready must not have marked the new worker ready');
});

test('cancelling a small render keeps the worker and tells it to drop the job', async () => {
  const hl = await load();
  const ctl = new AbortController();
  const out = hl.highlightPre('a.go', file(), ctl.signal);
  const [w] = workers;
  const [msg] = w.renders();
  ctl.abort();
  assert.deepEqual(await out, SKIPPED);
  assert.equal(w.terminated, false);
  assert.deepEqual(w.sent.at(-1), { type: 'cancel', id: msg.id });
  w.reply({ type: 'rendered', id: msg.id, html: 'late', styles: [STYLE] });
  assert.deepEqual(sheet.cssRules, [], 'a late reply must not add styles');
});

test('a render the worker reports as failed yields null and keeps the worker', async () => {
  const hl = await load();
  const out = hl.highlightPre('a.go', file(), new AbortController().signal);
  const [w] = workers;
  w.reply({ type: 'ready' });
  w.reply({ type: 'error', id: w.renders()[0].id, error: 'grammar chunk failed to load' });
  assert.deepEqual(await out, FAILED);
  assert.equal(w.terminated, false);
  assert.equal(hl.shikiAvailable('a.go'), true);
});

test('replies without a known type are ignored', async () => {
  const hl = await load();
  const ctl = new AbortController();
  const out = hl.highlightPre('a.go', file(), ctl.signal);
  const [w] = workers;
  w.reply({ id: w.renders()[0].id, html: 'untyped', styles: [] });
  assert.equal(await Promise.race([out, tick().then(() => 'pending')]), 'pending');
  ctl.abort();
  assert.deepEqual(await out, SKIPPED);
});

test('cancelling a large render terminates the worker', async () => {
  const hl = await load();
  const ctl = new AbortController();
  const out = hl.highlightPre('a.go', file('x\n'.repeat(3000), 3000), ctl.signal);
  ctl.abort();
  assert.deepEqual(await out, SKIPPED);
  assert.equal(workers[0].terminated, true);
  assert.equal(hl.shikiAvailable('a.go'), true);
});

test('a render the worker never answers times out and restarts the worker', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const hl = await load();
  const out = hl.highlightPre('a.go', file(), new AbortController().signal);
  workers[0].reply({ type: 'ready' });
  t.mock.timers.tick(60_000);
  assert.deepEqual(await out, FAILED);
  assert.equal(workers[0].terminated, true);
  assert.equal(hl.shikiAvailable('a.go'), true);
  const ctl = new AbortController();
  const next = hl.highlightPre('a.go', file(), ctl.signal);
  assert.equal(workers.length, 2, 'the next render starts a fresh worker');
  ctl.abort();
  assert.deepEqual(await next, SKIPPED);
});

test('a worker that hangs before it is ready hands highlighting back to the server', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const hl = await load();
  const out = hl.highlightPre('a.go', file(), new AbortController().signal);
  t.mock.timers.tick(60_000);
  assert.deepEqual(await out, FAILED);
  assert.equal(hl.shikiAvailable('a.go'), false);
});

test('preloading never throws', async () => {
  const hl = await load();
  globalThis.Worker = class { constructor() { throw new DOMException('blocked', 'SecurityError'); } };
  assert.doesNotThrow(() => hl.preloadGrammar('a.go'));
  assert.equal(hl.shikiAvailable('a.go'), false);
  await tick();
});
