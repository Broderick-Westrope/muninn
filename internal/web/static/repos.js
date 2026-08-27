'use strict';

import { api } from './api.js';
import { plural } from './dom.js';

// loadRepos fills the header badge with the indexed repo count and index
// age. The badge is informational, so failures leave it empty.
export async function loadRepos(repoBadge) {
  let repos;
  try {
    repos = await api('/api/repos');
  } catch {
    return;
  }
  const n = repos.length;
  const age = repos[0]?.indexAge;
  repoBadge.textContent =
    `${n} ${plural(n, 'repo', 'repos')}` +
    (age && age !== 'unknown' ? ` · indexed ${age} ago` : '');
  if (repos.some((r) => r.stale)) {
    const w = document.createElement('span');
    w.className = 'stale';
    w.title = 'Index older than 24h (or never synced) — run muninn sync';
    w.textContent = 'stale';
    repoBadge.append(' ', w);
  }
}
