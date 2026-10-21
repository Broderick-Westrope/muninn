import assert from 'node:assert/strict';
import { beforeEach, test } from 'node:test';

// Runs highlight-worker.js against the real vendored Shiki bundle with a
// fake worker global; each test imports a fresh copy of the worker module.

let posted;

beforeEach(() => {
  posted = [];
  globalThis.self = { postMessage: (msg) => posted.push(msg) };
});

let fresh = 0;
async function load() {
  await import(`../highlight-worker.js?t=${fresh++}`);
  const handle = globalThis.self.onmessage;
  return (data) => handle({ data });
}

const until = async (pred) => {
  for (let i = 0; i < 200 && !pred(); i++) await new Promise((r) => setTimeout(r, 5));
  assert.ok(pred(), 'condition not met in time');
};
const settle = () => new Promise((r) => setTimeout(r, 50));

test('reports ready once its highlighter exists', async () => {
  await load();
  await until(() => posted.some((m) => m.type === 'ready'));
});

test('replies to a render with typed, escaped markup', async () => {
  const send = await load();
  send({ type: 'render', id: 7, lang: 'go', content: 'package x // <b>\n' });
  await until(() => posted.some((m) => m.id === 7));
  const reply = posted.find((m) => m.id === 7);
  assert.equal(reply.type, 'rendered');
  assert.match(reply.html, /&lt;b&gt;/);
  assert.ok(reply.styles.length > 0);
});

test('a render for a grammar it does not bundle replies with an error', async () => {
  const send = await load();
  send({ type: 'render', id: 1, lang: 'constructor', content: 'x' });
  await until(() => posted.some((m) => m.id === 1));
  assert.equal(posted.find((m) => m.id === 1).type, 'error');
});

test('a cancel that arrives after the render message still drops it', async () => {
  const send = await load();
  send({ type: 'render', id: 1, lang: 'go', content: 'package x\n' });
  await until(() => posted.some((m) => m.id === 1));
  // The grammar is now loaded, so only a yield before tokenizing lets a
  // cancel queued as a separate task be seen in time.
  send({ type: 'render', id: 2, lang: 'go', content: 'package y\n' });
  setTimeout(() => send({ type: 'cancel', id: 2 }), 0);
  await settle();
  assert.equal(posted.some((m) => m.id === 2), false);
});

test('a cancel for a finished render is ignored', async () => {
  const send = await load();
  send({ type: 'render', id: 3, lang: 'go', content: 'package x\n' });
  await until(() => posted.some((m) => m.id === 3));
  send({ type: 'cancel', id: 3 });
  send({ type: 'render', id: 4, lang: 'go', content: 'package x\n' });
  await until(() => posted.some((m) => m.id === 4));
});
