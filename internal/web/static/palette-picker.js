'use strict';

// createPicker owns the palette's selection and Enter handling, apart from
// the DOM: which row is selected, whether the user chose it, and whether
// an Enter is waiting for a lookup. palette.js reports what it rendered and
// what the user did; the picker answers which row to select or run.
//
// The rules it keeps:
//   - New input selects the top row. So do async re-renders (results
//     arriving) until the user moves the selection; after that the moved
//     row stays selected, found again by key.
//   - Enter runs at once unless a lookup is pending and the selection sits
//     where its rows will land. Then it waits, and once results arrive it
//     runs the first of the lookup's rows, or nothing if there are none or
//     the lookup failed: never a fallback row the user did not see.
//   - Moving the selection or editing the input cancels a waiting Enter.
export function createPicker() {
  let keys = [];
  let active = 0;
  let navigated = false;
  let queued = false;
  let lookupStart = Infinity;
  let lookupCount = 0;

  const wrap = (i) => (keys.length ? (i + keys.length) % keys.length : 0);

  return {
    active: () => active,

    input() {
      navigated = false;
      queued = false;
    },

    // render takes the rows just drawn: their keys, and where the lookup's
    // rows start and how many there are. reset is true for an input
    // change. It returns the row to select.
    render(r) {
      const prevKey = keys[active];
      keys = r.keys;
      lookupStart = r.lookupStart;
      lookupCount = r.lookupCount;
      const i = r.reset || !navigated ? -1 : keys.indexOf(prevKey);
      active = i === -1 ? 0 : i;
      return active;
    },

    move(delta) {
      navigated = true;
      queued = false;
      active = wrap(active + delta);
      return active;
    },

    hover(i) {
      navigated = true;
      queued = false;
      active = wrap(i);
      return active;
    },

    // enter returns 'run' to run the selected row now, or 'queue' to wait
    // for the pending lookup.
    enter({ pending }) {
      if (pending && !navigated && active >= lookupStart) {
        queued = true;
        return 'queue';
      }
      return 'run';
    },

    // settle is called after the re-render for a finished lookup. It
    // returns the row a waiting Enter should run, or -1.
    settle({ ok }) {
      if (!queued) return -1;
      queued = false;
      if (!ok || lookupCount === 0) return -1;
      return active >= lookupStart && active < lookupStart + lookupCount ? active : -1;
    },

    close() {
      queued = false;
      navigated = false;
    },
  };
}
