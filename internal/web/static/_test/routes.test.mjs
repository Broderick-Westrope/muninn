import assert from 'node:assert/strict';
import { test } from 'node:test';

import { fileHash, parseFileHash, parseLineRange } from '../routes.js';

test('parseLineRange reads lines and ranges, normalising reversed ones', () => {
  const cases = [
    ['15', { line: 15, endLine: 0 }],
    ['10-20', { line: 10, endLine: 20 }],
    ['20-10', { line: 10, endLine: 20 }],
    ['5-5', { line: 5, endLine: 0 }],
    ['10 - 20', { line: 10, endLine: 20 }],
  ];
  for (const [input, want] of cases) assert.deepEqual(parseLineRange(input), want, input);
});

test('parseLineRange rejects anything that is not a positive line target', () => {
  for (const input of ['', '0', '5-0', '0-5', 'abc', '5-', '-5', '1.5']) {
    assert.equal(parseLineRange(input), null, input);
  }
});

test('file hashes round-trip a line range', () => {
  const hash = fileHash('o/r', 'a b/c.ts', 10, 20);
  assert.deepEqual(parseFileHash(hash), { repo: 'o/r', path: 'a b/c.ts', line: 10, endLine: 20 });
});

test('parseFileHash treats an invalid line suffix as no line', () => {
  assert.deepEqual(parseFileHash('#/file/o/r/c.ts:5-0'), { repo: 'o/r', path: 'c.ts', line: 0, endLine: 0 });
});
