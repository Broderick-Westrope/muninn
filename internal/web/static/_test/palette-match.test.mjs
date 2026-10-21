import assert from 'node:assert/strict';
import { test } from 'node:test';

import { fuzzy, keepActive, keyIntent, lookupQuery, mergeFiles, splitLineTarget } from '../palette-match.js';

test('keepActive keeps the selected item when an async update re-renders', () => {
  assert.equal(keepActive('cmd:permalink', ['cmd:focus', 'cmd:permalink', 'file:a'], false), 1);
});

test('keepActive resets to the top when the input changed', () => {
  assert.equal(keepActive('cmd:permalink', ['cmd:focus', 'cmd:permalink'], true), 0);
});

test('keepActive falls back to the top when the selection is gone', () => {
  assert.equal(keepActive('cmd:gone', ['cmd:focus', 'cmd:permalink'], false), 0);
});

test('mergeFiles puts current-repo matches first without duplicates', () => {
  const scoped = [{ repo: 'o/cur', path: 'src/index.ts' }];
  const global = [
    { repo: 'o/other', path: 'index.ts' },
    { repo: 'o/cur', path: 'src/index.ts' },
  ];
  assert.deepEqual(mergeFiles(scoped, global), [
    { repo: 'o/cur', path: 'src/index.ts' },
    { repo: 'o/other', path: 'index.ts' },
  ]);
});

// Characterisation tests for behaviour moved out of palette.js.

test('splitLineTarget reads a trailing line or range off a file term', () => {
  assert.deepEqual(splitLineTarget('payments.ts:15'), { base: 'payments.ts', target: { line: 15, endLine: 0 } });
  assert.deepEqual(splitLineTarget('payments.ts:30-20'), {
    base: 'payments.ts',
    target: { line: 20, endLine: 30 },
  });
});

test('splitLineTarget leaves a term without a numeric suffix alone', () => {
  assert.deepEqual(splitLineTarget('repo:foo'), { base: 'repo:foo', target: null });
  assert.deepEqual(splitLineTarget(':15'), { base: ':15', target: null });
});

test('fuzzy accepts word prefixes but not letters scattered mid-word', () => {
  assert.ok(fuzzy('Go to symbol…', 'gts'));
  assert.equal(fuzzy('eucalyptusvc/erx-cloud-vpn-infra', 'verif'), null);
});

test('lookupQuery escapes regex metacharacters and ANDs tokens', () => {
  assert.equal(lookupQuery('file', 'payments.ts auth'), 'file:payments\\.ts file:auth type:file');
  assert.equal(lookupQuery('sym', 'New(Server'), 'sym:New\\(Server');
});

const key = (k, mods = {}) => ({ key: k, metaKey: false, ctrlKey: false, altKey: false, shiftKey: false, isComposing: false, keyCode: 0, ...mods });

test('keyIntent maps palette keys to actions', () => {
  assert.equal(keyIntent(key('Enter'), true), 'run');
  assert.equal(keyIntent(key('ArrowDown'), true), 'down');
  assert.equal(keyIntent(key('p', { ctrlKey: true }), true), 'up');
  assert.equal(keyIntent(key('k', { metaKey: true }), true), 'close');
  assert.equal(keyIntent(key('g', { metaKey: true }), true), 'goToLine');
  assert.equal(keyIntent(key('g', { ctrlKey: true }), false), 'goToLine');
  assert.equal(keyIntent(key('a'), true), null);
});

test('keyIntent leaves keys alone while an IME is composing', () => {
  assert.equal(keyIntent(key('Enter', { isComposing: true }), true), null);
  // Safari reports the composition-ending keydown with keyCode 229 only.
  assert.equal(keyIntent(key('Enter', { keyCode: 229 }), true), null);
  assert.equal(keyIntent(key('Escape', { isComposing: true }), true), null);
});
