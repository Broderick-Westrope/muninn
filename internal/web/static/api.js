'use strict';

// api fetches path as JSON, throwing an Error carrying the HTTP status and
// the server's error message (when the body is JSON).
export async function api(path, signal) {
  const res = await fetch(path, { signal });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      msg = (await res.json()).error || msg;
    } catch {
      /* non-JSON error body */
    }
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  return res.json();
}
