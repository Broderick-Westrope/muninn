// Entry point for the vendored Shiki bundle in internal/web/static/vendor/shiki.
// Bundle it with build.mjs, which first writes the generated/ modules this
// imports. Each grammar is a dynamic import so esbuild splits it into its
// own chunk and the browser only fetches the languages it renders.
import { createHighlighterCore } from 'shiki/core';
import { createJavaScriptRegexEngine } from 'shiki/engine/javascript';
import anvilNight from './generated/anvil-night.json' with { type: 'json' };
import dayPalette from './generated/day-palette.json' with { type: 'json' };

export { LANGS } from './generated/langs.mjs';

export const THEME = anvilNight.name;

// DAY_PALETTE maps each anvil-night token colour (lower-case #rrggbb) to its
// light-mode colour; see dayPalette in build.mjs.
export const DAY_PALETTE = dayPalette;

let highlighter = null;

// getHighlighter creates the shared highlighter on first use with no
// languages loaded; callers load grammars on demand via loadLanguage. A
// failed creation is retried on the next call rather than cached.
export function getHighlighter() {
  if (!highlighter) {
    highlighter = createHighlighterCore({
      themes: [anvilNight],
      langs: [],
      engine: createJavaScriptRegexEngine(),
    });
    highlighter.catch(() => {
      highlighter = null;
    });
  }
  return highlighter;
}
