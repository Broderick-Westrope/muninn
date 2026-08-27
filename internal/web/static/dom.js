'use strict';

export const $ = (sel, el = document) => el.querySelector(sel);

export const plural = (n, one, many) => (n === 1 ? one : many);

const COPY_FEEDBACK_MS = 1000;

// ---------------------------------------------------------------------------
// Match highlighting

// markMatches renders line text into el, wrapping case-insensitive
// occurrences of tokens in <mark>. Built with text nodes, never innerHTML.
export function markMatches(el, text, tokens) {
  const lower = text.toLowerCase();
  const ranges = [];
  for (const t of tokens) {
    const lt = t.toLowerCase();
    let i = 0;
    while (lt && (i = lower.indexOf(lt, i)) !== -1) {
      ranges.push([i, i + lt.length]);
      i += lt.length;
    }
  }
  ranges.sort((a, b) => a[0] - b[0]);
  const merged = [];
  for (const r of ranges) {
    const last = merged[merged.length - 1];
    if (last && r[0] <= last[1]) last[1] = Math.max(last[1], r[1]);
    else merged.push(r);
  }
  let pos = 0;
  for (const [start, end] of merged) {
    if (start > pos) el.append(text.slice(pos, start));
    const mark = document.createElement('mark');
    mark.textContent = text.slice(start, end);
    el.append(mark);
    pos = end;
  }
  if (pos < text.length) el.append(text.slice(pos));
}

// ---------------------------------------------------------------------------
// Copy affordances (inline SVG, currentColor so themes work; icon markup is
// static — user data still flows through textContent everywhere)

const ICONS = {
  copy:
    '<path fill="none" stroke="currentColor" stroke-width="1.5" d="M5.75 5.75h7.5a1 1 0 0 1 1 1v7.5a1 1 0 0 1-1 1h-7.5a1 1 0 0 1-1-1v-7.5a1 1 0 0 1 1-1Z"/>' +
    '<path fill="none" stroke="currentColor" stroke-width="1.5" d="M10.25 5.75v-3a1 1 0 0 0-1-1h-6.5a1 1 0 0 0-1 1v6.5a1 1 0 0 0 1 1h3"/>',
  check: '<path fill="none" stroke="currentColor" stroke-width="2" d="m2.75 8.75 3.5 3.5 7-8.5"/>',
  link:
    '<path fill="none" stroke="currentColor" stroke-width="1.5" d="M6.5 9.5a3 3 0 0 0 4.4.2l2.2-2.2a3 3 0 1 0-4.3-4.2L7.6 4.5"/>' +
    '<path fill="none" stroke="currentColor" stroke-width="1.5" d="M9.5 6.5a3 3 0 0 0-4.4-.2L2.9 8.5a3 3 0 1 0 4.3 4.2l1.2-1.2"/>',
};

export function icon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 16 16');
  svg.setAttribute('aria-hidden', 'true');
  svg.innerHTML = ICONS[name]; // static markup from ICONS only, never user data
  return svg;
}

// copyBtn returns an icon button that copies text (a string, or a function
// evaluated at click time) and briefly swaps to a checkmark to confirm.
export function copyBtn(text, title, iconName = 'copy') {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'copy-btn';
  b.title = title;
  b.setAttribute('aria-label', title);
  b.append(icon(iconName));
  let timer = 0;
  b.addEventListener('click', async (e) => {
    // Copy buttons live inside result-row anchors; never navigate.
    e.preventDefault();
    e.stopPropagation();
    try {
      await navigator.clipboard.writeText(typeof text === 'function' ? text() : text);
    } catch {
      return; // clipboard unavailable — leave the button inert
    }
    b.replaceChildren(icon('check'));
    b.classList.add('copied');
    clearTimeout(timer);
    timer = setTimeout(() => {
      b.replaceChildren(icon(iconName));
      b.classList.remove('copied');
    }, COPY_FEEDBACK_MS);
  });
  return b;
}

// ---------------------------------------------------------------------------
// Message boxes

export function note(text) {
  const div = document.createElement('div');
  div.className = 'note';
  div.textContent = text;
  return div;
}

export function errorBox(msg) {
  const div = document.createElement('div');
  div.className = 'error';
  div.textContent = msg;
  return div;
}
