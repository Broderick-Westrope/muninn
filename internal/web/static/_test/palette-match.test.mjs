import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
  fuzzy,
  isGoToLineKey,
  isPaletteKey,
  keyIntent,
  lookupQuery,
  mergeFiles,
  parse,
  PREFIXES,
  splitLineTarget,
} from '../palette-match.js';

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

test('lookupQuery passes repo: atoms through to scope a lookup', () => {
  assert.equal(lookupQuery('file', 'repo:pilot payments.ts'), 'repo:pilot file:payments\\.ts type:file');
});

test('splitLineTarget drops a trailing colon typed before the line number', () => {
  assert.deepEqual(splitLineTarget('main.go:'), { base: 'main.go', target: null });
});

test('isPaletteKey is Cmd+K on a Mac and Ctrl+K elsewhere, with no other modifiers', () => {
  assert.equal(isPaletteKey(key('k', { metaKey: true }), true), true);
  assert.equal(isPaletteKey(key('k', { ctrlKey: true }), true), false);
  assert.equal(isPaletteKey(key('k', { ctrlKey: true }), false), true);
  assert.equal(isPaletteKey(key('k', { metaKey: true, shiftKey: true }), true), false);
});

test('keyIntent closes only on the palette chord, not its variants', () => {
  assert.equal(keyIntent(key('k', { ctrlKey: true }), true), null);
  assert.equal(keyIntent(key('k', { metaKey: true, shiftKey: true }), true), null);
});

test('Ctrl+N and Ctrl+P navigate only on a Mac, where they are not browser shortcuts', () => {
  assert.equal(keyIntent(key('n', { ctrlKey: true }), false), null);
  assert.equal(keyIntent(key('p', { ctrlKey: true }), false), null);
});

test('chords tolerate a keydown with no key, as autofill sends', () => {
  const bare = key(undefined);
  assert.equal(keyIntent(bare, true), null);
  assert.equal(isPaletteKey(bare, true), false);
  assert.equal(isGoToLineKey(bare, true), false);
});

test('splitLineTarget keeps the file while a range is half typed', () => {
  assert.deepEqual(splitLineTarget('payments.ts:10-'), { base: 'payments.ts', target: { line: 10, endLine: 0 } });
});

test('splitLineTarget ignores a trailing column, as compilers print', () => {
  assert.deepEqual(splitLineTarget('foo.go:10:5'), { base: 'foo.go', target: { line: 10, endLine: 0 } });
});

test('parse reads each prefix in PREFIXES as its mode', () => {
  for (const p of PREFIXES) assert.deepEqual(parse(`${p.char} x `), { mode: p.mode, term: 'x' });
  assert.deepEqual(parse(' x '), { mode: 'all', term: 'x' });
});
