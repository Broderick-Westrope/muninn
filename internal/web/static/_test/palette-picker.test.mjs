import assert from 'node:assert/strict';
import { test } from 'node:test';

import { createPicker } from '../palette-picker.js';

// Rows as palette.js renders them: commands first, then lookup rows.
const withFiles = { keys: ['cmd:a', 'cmd:b', 'file:x', 'file:y'], lookupStart: 2, lookupCount: 2 };
const filesOnly = { keys: ['file:x', 'file:y', 'search'], lookupStart: 0, lookupCount: 2 };

test('an input change selects the top row', () => {
  const p = createPicker();
  p.render({ ...withFiles, reset: true });
  p.move(2);
  p.input();
  assert.equal(p.render({ ...withFiles, reset: true }), 0);
});

test('an async re-render keeps a row the user moved to', () => {
  const p = createPicker();
  p.render({ ...withFiles, reset: true });
  p.move(1);
  const after = { keys: ['cmd:x', 'cmd:a', 'cmd:b', 'file:x'], lookupStart: 3, lookupCount: 1 };
  assert.equal(p.render({ ...after, reset: false }), 2);
});

test('an async re-render returns to the top when the user has not moved', () => {
  const p = createPicker();
  p.render({ keys: ['search'], lookupStart: 0, lookupCount: 0, reset: true });
  assert.equal(p.render({ ...filesOnly, reset: false }), 0);
});

test('move wraps around the rows', () => {
  const p = createPicker();
  p.render({ ...withFiles, reset: true });
  assert.equal(p.move(-1), 3);
  assert.equal(p.move(1), 0);
});

test('Enter runs at once when no lookup is pending', () => {
  const p = createPicker();
  p.render({ ...filesOnly, reset: true });
  assert.equal(p.enter({ pending: false }), 'run');
});

test('Enter on a command above pending lookup rows runs at once', () => {
  const p = createPicker();
  p.render({ ...withFiles, reset: true });
  assert.equal(p.enter({ pending: true }), 'run');
});

test('Enter where pending lookup rows will land waits, then runs the first of them', () => {
  const p = createPicker();
  p.render({ keys: ['search'], lookupStart: 0, lookupCount: 0, reset: true });
  assert.equal(p.enter({ pending: true }), 'queue');

  p.render({ ...filesOnly, reset: false });
  assert.equal(p.settle({ ok: true }), 0);
});

test('a queued Enter with no lookup rows runs the row it was pressed on, still selected', () => {
  const p = createPicker();
  const pending = { keys: ['search'], lookupStart: 0, lookupCount: 0 };
  p.render({ ...pending, reset: true });
  p.enter({ pending: true });

  p.render({ ...pending, reset: false });
  assert.equal(p.settle({ ok: true }), 0);
});

test('a queued Enter with no lookup rows runs nothing if a different row is now on top', () => {
  const p = createPicker();
  p.render({ keys: ['search'], lookupStart: 0, lookupCount: 0, reset: true });
  p.enter({ pending: true });

  p.render({ keys: ['cmd:clear', 'search'], lookupStart: 0, lookupCount: 0, reset: false });
  assert.equal(p.settle({ ok: true }), -1);
});

test('a queued Enter runs nothing when the lookup failed', () => {
  const p = createPicker();
  p.render({ keys: ['search'], lookupStart: 0, lookupCount: 0, reset: true });
  p.enter({ pending: true });

  assert.equal(p.settle({ ok: false }), -1);
});

test('moving the selection cancels a queued Enter', () => {
  const p = createPicker();
  p.render({ keys: ['search'], lookupStart: 0, lookupCount: 0, reset: true });
  p.enter({ pending: true });
  p.move(1);

  p.render({ ...filesOnly, reset: false });
  assert.equal(p.settle({ ok: true }), -1);
});

test('after the user moves, Enter runs their row even while a lookup is pending', () => {
  const p = createPicker();
  p.render({ ...filesOnly, reset: true });
  p.move(1);
  assert.equal(p.enter({ pending: true }), 'run');
});

test('a queued Enter with nothing to select runs nothing', () => {
  const p = createPicker();
  p.render({ keys: [], lookupStart: 0, lookupCount: 0, reset: true });
  p.enter({ pending: true });

  p.render({ keys: [], lookupStart: 0, lookupCount: 0, reset: false });
  assert.equal(p.settle({ ok: true }), -1);
});
