'use strict';

// The hash-URL format for the file view, owned in one place: fileHash writes
// it and parseFileHash reads it, so changing one without the other is not
// possible. Both search results and the file tree link through fileHash, so
// navigation always goes via the router.

export function fileHash(repo, path, line) {
  const p = path.split('/').map(encodeURIComponent).join('/');
  return `#/file/${repo}/${p}${line ? ':' + line : ''}`;
}

// parseFileHash parses "#/file/<owner>/<name>/<path>[:<line>]".
export function parseFileHash(hash) {
  let rest = hash.slice('#/file/'.length);
  let line = 0;
  const m = rest.match(/:(\d+)$/);
  if (m) {
    line = Number(m[1]);
    rest = rest.slice(0, -m[0].length);
  }
  const segs = rest.split('/').map(decodeURIComponent);
  if (segs.length < 3 || !segs[0] || !segs[1]) return null;
  return { repo: `${segs[0]}/${segs[1]}`, path: segs.slice(2).join('/'), line };
}
