'use strict';

// The hash-URL format for the file view, owned in one place: fileHash writes
// it and parseFileHash reads it, so changing one without the other is not
// possible. Both search results and the file tree link through fileHash, so
// navigation always goes via the router.

// lineSuffix renders a target as ":<line>" or ":<start>-<end>"; endLine is
// only written when it extends past line.
function lineSuffix(line, endLine) {
  if (!line) return '';
  return endLine > line ? `:${line}-${endLine}` : `:${line}`;
}

export function fileHash(repo, path, line, endLine) {
  const p = path.split('/').map(encodeURIComponent).join('/');
  return `#/file/${repo}/${p}${lineSuffix(line, endLine)}`;
}

// parseFileHash parses "#/file/<owner>/<name>/<path>[:<line>[-<endLine>]]".
// endLine is 0 for a single-line (or no) target, and a reversed range is
// normalised so line is always the first line.
export function parseFileHash(hash) {
  let rest = hash.slice('#/file/'.length);
  let line = 0;
  let endLine = 0;
  const m = rest.match(/:(\d+)(?:-(\d+))?$/);
  if (m) {
    line = Number(m[1]);
    endLine = m[2] ? Number(m[2]) : 0;
    if (endLine && endLine < line) [line, endLine] = [endLine, line];
    if (endLine === line) endLine = 0;
    rest = rest.slice(0, -m[0].length);
  }
  const segs = rest.split('/').map(decodeURIComponent);
  if (segs.length < 3 || !segs[0] || !segs[1]) return null;
  return { repo: `${segs[0]}/${segs[1]}`, path: segs.slice(2).join('/'), line, endLine };
}
