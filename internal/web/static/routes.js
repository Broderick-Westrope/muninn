'use strict';

// The hash-URL format for the file view, owned in one place: fileHash writes
// it and parseFileHash reads it, so changing one without the other is not
// possible. Both search results and the file tree link through fileHash, so
// navigation always goes via the router. The palette's line targets
// ("payments.ts:15", ":10-20") use the same line-range syntax, so it lives
// here too.

// parseLineRange reads "15" or "10-20" as {line, endLine}, normalising a
// reversed range; endLine is 0 for a single line. null unless every line
// is positive.
export function parseLineRange(s) {
  const m = s.match(/^(\d+)(?:\s*-\s*(\d+))?$/);
  if (!m) return null;
  let line = Number(m[1]);
  let endLine = m[2] === undefined ? 0 : Number(m[2]);
  if (line < 1 || (m[2] !== undefined && endLine < 1)) return null;
  if (endLine && endLine < line) [line, endLine] = [endLine, line];
  if (endLine === line) endLine = 0;
  return { line, endLine };
}

// lineSuffix renders a target as ":<line>" or ":<start>-<end>"; endLine is
// only written when it extends past line.
export function lineSuffix(line, endLine) {
  if (!line) return '';
  return endLine > line ? `:${line}-${endLine}` : `:${line}`;
}

export function fileHash(repo, path, line, endLine) {
  const p = path.split('/').map(encodeURIComponent).join('/');
  return `#/file/${repo}/${p}${lineSuffix(line, endLine)}`;
}

// parseFileHash parses "#/file/<owner>/<name>/<path>[:<line>[-<endLine>]]".
// endLine is 0 for a single-line (or no) target.
export function parseFileHash(hash) {
  let rest = hash.slice('#/file/'.length);
  let line = 0;
  let endLine = 0;
  const m = rest.match(/:(\d+(?:-\d+)?)$/);
  if (m) {
    ({ line, endLine } = parseLineRange(m[1]) || { line: 0, endLine: 0 });
    rest = rest.slice(0, -m[0].length);
  }
  const segs = rest.split('/').map(decodeURIComponent);
  if (segs.length < 3 || !segs[0] || !segs[1]) return null;
  return { repo: `${segs[0]}/${segs[1]}`, path: segs.slice(2).join('/'), line, endLine };
}
