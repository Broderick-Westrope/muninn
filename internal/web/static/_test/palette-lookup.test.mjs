import assert from 'node:assert/strict';
import { test } from 'node:test';

import { createLookup } from '../palette-lookup.js';

// deferredFetch records each request and lets the test settle them in any
// order, ignoring abort like a response already on the wire.
function deferredFetch() {
  const calls = [];
  const fetch = (args) =>
    new Promise((resolve, reject) => {
      calls.push({ args, resolve, reject });
    });
  return { calls, fetch };
}

const tick = () => new Promise((r) => setTimeout(r, 0));

test('a pending lookup shows the previous results as stale, never fresh', async () => {
  const f = deferredFetch();
  const lookup = createLookup({ fetch: f.fetch, debounceMs: 0, onSettle: () => {} });
  lookup.view('old', 'old');
  await tick();
  f.calls[0].resolve({ items: ['old.ts'], truncated: false });
  await tick();

  const v = lookup.view('new', 'new');

  assert.equal(v.fresh, false);
  assert.equal(v.pending, true);
  assert.deepEqual(v.items, []);
  assert.deepEqual(v.stale, ['old.ts']);
});

test('a slow response for a superseded key never replaces the current one', async () => {
  const f = deferredFetch();
  const lookup = createLookup({ fetch: f.fetch, debounceMs: 0, onSettle: () => {} });
  lookup.view('a', 'a');
  await tick();
  lookup.view('b', 'b');
  await tick();

  f.calls[1].resolve({ items: ['b.ts'], truncated: false });
  await tick();
  f.calls[0].resolve({ items: ['a.ts'], truncated: false });
  await tick();

  const v = lookup.view('b', 'b');
  assert.equal(v.fresh, true);
  assert.deepEqual(v.items, ['b.ts']);
});

test('re-rendering the same key does not restart its request', async () => {
  const f = deferredFetch();
  const lookup = createLookup({ fetch: f.fetch, debounceMs: 0, onSettle: () => {} });
  lookup.view('a', 'a');
  lookup.view('a', 'a');
  await tick();
  lookup.view('a', 'a');
  await tick();

  assert.equal(f.calls.length, 1);
});

test('an empty key cancels the pending lookup', async () => {
  const f = deferredFetch();
  let settled = 0;
  const lookup = createLookup({ fetch: f.fetch, debounceMs: 0, onSettle: () => settled++ });
  lookup.view('a', 'a');
  await tick();

  const v = lookup.view('', null);
  f.calls[0].resolve({ items: ['a.ts'], truncated: false });
  await tick();

  assert.equal(v.pending, false);
  assert.equal(lookup.pending(), false);
  assert.equal(settled, 0);
});

test('a failed lookup is fresh with its error', async () => {
  const f = deferredFetch();
  const lookup = createLookup({ fetch: f.fetch, debounceMs: 0, onSettle: () => {} });
  lookup.view('a', 'a');
  await tick();
  f.calls[0].reject(new Error('bad query'));
  await tick();

  const v = lookup.view('a', 'a');
  assert.equal(v.fresh, true);
  assert.equal(v.error, 'bad query');
  assert.deepEqual(v.items, []);
});
