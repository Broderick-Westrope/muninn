'use strict';

// Which vendored Shiki grammar renders a file. This module is the single
// owner of the language set: tools/shiki/build.mjs bundles exactly the
// grammars in LANGUAGES. It must stay free of DOM access so Node can test
// it and the build can import it.

const BY_EXT = new Map(Object.entries({
  c: 'c', h: 'c',
  cc: 'cpp', cpp: 'cpp', cxx: 'cpp', hh: 'cpp', hpp: 'cpp', hxx: 'cpp',
  cs: 'csharp',
  css: 'css',
  diff: 'diff', patch: 'diff',
  go: 'go',
  graphql: 'graphql', gql: 'graphql',
  hcl: 'hcl',
  html: 'html', htm: 'html',
  ini: 'ini', cfg: 'ini', properties: 'ini',
  java: 'java',
  js: 'javascript', mjs: 'javascript', cjs: 'javascript',
  json: 'json',
  jsonc: 'jsonc',
  jsx: 'jsx',
  kt: 'kotlin', kts: 'kotlin',
  lua: 'lua',
  mk: 'make',
  md: 'markdown', markdown: 'markdown',
  php: 'php',
  proto: 'proto',
  py: 'python', pyi: 'python',
  rb: 'ruby',
  rs: 'rust',
  scss: 'scss',
  sh: 'shellscript', bash: 'shellscript', zsh: 'shellscript',
  sql: 'sql',
  swift: 'swift',
  tf: 'terraform', tfvars: 'terraform',
  toml: 'toml',
  ts: 'typescript', mts: 'typescript', cts: 'typescript',
  tsx: 'tsx',
  vue: 'vue',
  xml: 'xml', svg: 'xml', plist: 'xml',
  yaml: 'yaml', yml: 'yaml',
}));

const BY_NAME = new Map(Object.entries({
  dockerfile: 'docker',
  makefile: 'make',
  gnumakefile: 'make',
  'tsconfig.json': 'jsonc',
  '.bashrc': 'shellscript',
  '.zshrc': 'shellscript',
}));

// LANGUAGES is every grammar id languageFor can return, sorted.
export const LANGUAGES = [...new Set([...BY_EXT.values(), ...BY_NAME.values()])].sort();

// languageFor picks a Shiki grammar from the file name, or null when the
// file is outside the vendored set (the server renders those with chroma).
export function languageFor(path) {
  const base = path.slice(path.lastIndexOf('/') + 1).toLowerCase();
  if (BY_NAME.has(base)) return BY_NAME.get(base);
  if (/^dockerfile\../.test(base)) return 'docker';
  const dot = base.lastIndexOf('.');
  return dot > 0 ? (BY_EXT.get(base.slice(dot + 1)) ?? null) : null;
}
