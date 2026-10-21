'use strict';

// createLookup tracks the palette's one remote lookup (files or symbols).
// Results are only ever reported as fresh for the key that requested them:
// while a newer key is in flight the previous results are offered as stale,
// for display only, so a fast Enter can never open a file from an older
// query.
//
// fetch(args, signal) resolves to {items, truncated}. onSettle({ok}) runs
// when a still-wanted lookup finishes, so the caller can re-render; ok is
// false when it failed.
export function createLookup({ fetch, debounceMs, onSettle }) {
  let done = { key: '', items: [], truncated: false, error: '' };
  let pendingKey = '';
  let timer = 0;
  let ctl = null;

  function abortPending() {
    clearTimeout(timer);
    if (ctl) ctl.abort();
    ctl = null;
    pendingKey = '';
  }

  function cancel() {
    abortPending();
    // A failure is kept only while its palette session lasts: re-rendering
    // must not retry it in a loop, but the next session should.
    if (done.error) done = { key: '', items: [], truncated: false, error: '' };
  }

  function start(key, args) {
    abortPending();
    pendingKey = key;
    timer = setTimeout(async () => {
      const mine = new AbortController();
      ctl = mine;
      let next;
      try {
        const r = await fetch(args, mine.signal);
        next = { key, items: r.items, truncated: r.truncated, error: '' };
      } catch (e) {
        if (e.name === 'AbortError') return;
        next = { key, items: [], truncated: false, error: e.message };
      }
      // A response already on the wire can outlive its abort.
      if (ctl !== mine) return;
      ctl = null;
      pendingKey = '';
      done = next;
      onSettle({ ok: !next.error });
    }, debounceMs);
  }

  return {
    // view reports what is known for key, starting its request when it is
    // neither loaded nor in flight. An empty key means no lookup is wanted
    // and cancels any pending one.
    view(key, args) {
      if (!key) {
        cancel();
        return { fresh: false, pending: false, items: [], stale: [], staleKey: '', truncated: false, error: '' };
      }
      const fresh = done.key === key;
      if (fresh && pendingKey) {
        // Back to a loaded key: a newer request in flight is no longer
        // wanted and must not replace these results.
        abortPending();
      } else if (!fresh && pendingKey !== key) {
        start(key, args);
      }
      return {
        fresh,
        pending: !fresh,
        items: fresh ? done.items : [],
        stale: fresh ? [] : done.items,
        staleKey: fresh ? '' : done.key,
        truncated: fresh && done.truncated,
        error: fresh ? done.error : '',
      };
    },
    pending: () => pendingKey !== '',
    cancel,
  };
}
