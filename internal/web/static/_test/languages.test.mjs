import assert from 'node:assert/strict';
import { test } from 'node:test';
import { languageFor } from '../languages.js';

test('maps extensions and file names to grammars', () => {
  assert.equal(languageFor('cmd/main.go'), 'go');
  assert.equal(languageFor('src/App.TSX'), 'tsx');
  assert.equal(languageFor('Dockerfile'), 'docker');
  assert.equal(languageFor('deploy/Dockerfile.prod'), 'docker');
  assert.equal(languageFor('tsconfig.json'), 'jsonc');
  assert.equal(languageFor('a/b/Makefile'), 'make');
});

test('leaves unknown and extension-less files to chroma', () => {
  assert.equal(languageFor('go.sum'), null);
  assert.equal(languageFor('LICENSE'), null);
  assert.equal(languageFor('.gitignore'), null);
  assert.equal(languageFor('go.mod'), null);
});

test('only Dockerfile and Dockerfile.<variant> are Docker', () => {
  assert.equal(languageFor('Dockerfile.dev'), 'docker');
  assert.equal(languageFor('dockerfile_helper.py'), 'python');
  assert.equal(languageFor('docs/dockerfile-notes.md'), 'markdown');
  assert.equal(languageFor('Dockerfiles'), null);
});

test('ignores names inherited from Object.prototype', () => {
  for (const name of ['constructor', 'toString', '__proto__', 'valueOf', 'hasOwnProperty']) {
    assert.equal(languageFor(name), null, name);
    assert.equal(languageFor(`x.${name}`), null, `x.${name}`);
  }
});
