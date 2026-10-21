'use strict';

// Turns file content into the file viewer's highlighted markup. It is the
// only code between repository content and the innerHTML in highlight.js,
// so it must escape every source character, and it stays free of DOM and
// worker APIs so Node can test it. It owns the shape of the token classes
// and colours it emits; highlight.js validates against the same patterns.

// TOKEN_CLASS is every class name highlightFile emits: sk-<hex>-<fontStyle>.
export const TOKEN_CLASS = /^sk-[0-9a-f]+-\d+$/;

// TOKEN_COLOR is every colour highlightFile emits: #rrggbb or #rrggbbaa.
export const TOKEN_COLOR = /^#[0-9a-f]{6}([0-9a-f]{2})?$/i;

// Lines longer than this render uncoloured: tokenizing one long minified
// line can otherwise hold the worker for seconds (VS Code's default).
const MAX_LINE_LENGTH = 20_000;

const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' };
const escape = (s) => s.replace(/[&<>"]/g, (c) => ESC[c]);

// Class names are derived from the style itself, so a restarted worker
// produces the same name for the same style and rules never collide.
const classFor = (color, fontStyle) => `sk-${color.slice(1).toLowerCase()}-${fontStyle}`;

// highlightFile tokenizes content with a Shiki highlighter that already has
// lang's grammar and theme loaded, and renders it in the DOM shape of
// plainPre in file.js (span.line > span.ln#Ln > a.lnlinks + span.cl). It
// returns the inner <code> markup and, for every token class it uses,
// [class, dark colour, light colour, fontStyle]. dayPalette maps each dark
// #rrggbb colour of theme to its light-mode colour.
export function highlightFile(hl, { lang, theme, dayPalette }, content) {
  const { tokens } = hl.codeToTokens(content.replace(/\n$/, ''), {
    lang,
    theme,
    tokenizeMaxLineLength: MAX_LINE_LENGTH,
  });
  const styles = new Map();
  const html = [];
  tokens.forEach((line, i) => {
    const n = i + 1;
    html.push(
      `<span class="line"><span class="ln" id="L${n}"><a class="lnlinks" href="#L${n}">${n}</a></span><span class="cl">`,
    );
    for (const t of line) {
      if (!t.color) {
        html.push(escape(t.content));
        continue;
      }
      const fontStyle = t.fontStyle > 0 ? t.fontStyle : 0;
      const cls = classFor(t.color, fontStyle);
      if (!styles.has(cls)) {
        const hex = t.color.slice(0, 7).toLowerCase();
        const light = (dayPalette[hex] ?? hex) + t.color.slice(7);
        styles.set(cls, [cls, t.color, light, fontStyle]);
      }
      html.push(`<span class="${cls}">${escape(t.content)}</span>`);
    }
    html.push('\n</span></span>');
  });
  return { html: html.join(''), styles: [...styles.values()] };
}
