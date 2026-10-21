// Regenerates the vendored Shiki bundle served by the web UI:
//
//   cd tools/shiki && npm ci && npm run build
//
// The Go build never runs this: the output in internal/web/static/vendor is
// committed so the binary stays a pure `go build` with no Node toolchain.
import { existsSync, mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { build } from 'esbuild';
import tokyoNight from '@shikijs/themes/tokyo-night';
import { grammars, injections } from 'tm-grammars';
import { themes } from 'tm-themes';
import { LANGUAGES } from '../../internal/web/static/languages.js';

const here = dirname(fileURLToPath(import.meta.url));
const out = join(here, '../../internal/web/static/vendor/shiki');

// Anvil Night is Tokyo Night on a near-black background with the brighter
// editor foreground and comment colour; the page owns the background, so
// only token colours matter here.
const nightRemap = {
  '#a9b1d6': '#c0caf5',
  '#51597d': '#565f89',
};

// Tokyo Night Day equivalents for every colour tokyo-night's token rules use,
// so light mode keeps the same token-to-hue assignments.
const dayRemap = {
  '#bb9af7': '#9854f1',
  '#c0caf5': '#3760bf',
  '#a9b1d6': '#3760bf',
  '#f7768e': '#f52a65',
  '#0db9d7': '#07879d',
  '#7aa2f7': '#2e7de9',
  '#7dcfff': '#007197',
  '#89ddff': '#006a83',
  '#e0af68': '#8c6c3e',
  '#73daca': '#387068',
  '#9ece6a': '#587539',
  '#9abdf5': '#4260a8',
  '#ff9e64': '#b15c00',
  '#9aa5ce': '#6172b0',
  '#51597d': '#848cb5',
  '#9d7cd8': '#7847bd',
  '#4e5579': '#a1a6c5',
  '#5a638c': '#8990b3',
  '#646e9c': '#848cb5',
  '#ff5370': '#f52a65',
  '#de5971': '#c64343',
  '#ba3c97': '#b03d8b',
  '#41a6b5': '#2c7c87',
  '#fc7b7b': '#d63c3c',
  '#449dab': '#2c7c87',
  '#914c54': '#914c54',
  '#6183bb': '#4f6aa8',
  '#b4f9f8': '#3e6968',
  '#61bdf2': '#1f73b8',
  '#6d91de': '#3e68c9',
  '#747ca1': '#6c76a3',
  '#c0cefc': '#3760bf',
  '#ffdb69': '#8c6c3e',
  '#db4b4b': '#c64343',
  '#b267e6': '#8f4fd1',
};

// remapTheme deep-copies a theme, swapping each #rrggbb (keeping any alpha
// suffix) through the table. Colours the table lacks pass through unchanged.
function remapTheme(theme, name, type, table) {
  const swap = (v) =>
    v.replace(/#[0-9a-f]{6}/gi, (hex) => table[hex.toLowerCase()] ?? hex);
  const walk = (v) => {
    if (typeof v === 'string') return swap(v);
    if (Array.isArray(v)) return v.map(walk);
    if (v && typeof v === 'object') {
      return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, walk(x)]));
    }
    return v;
  };
  return { ...walk(theme), name, displayName: name, type };
}

// dayPalette maps every colour anvil-night can give a token to its Day
// equivalent. Both themes share tokyo-night's rules and differ only in
// colour, so the page tokenizes once with anvil-night and recolours for
// light mode with this table instead of tokenizing a second time.
function dayPalette(theme) {
  const sources = new Set([theme.colors['editor.foreground']]);
  for (const rule of theme.tokenColors) {
    if (rule.settings.foreground) sources.add(rule.settings.foreground);
  }
  const palette = {};
  for (const src of sources) {
    const base = src.toLowerCase().slice(0, 7);
    const night = nightRemap[base] ?? base;
    const day = dayRemap[base];
    if (!day) throw new Error(`tokyo-night colour ${base} has no Day equivalent in dayRemap`);
    if (palette[night] && palette[night] !== day) {
      throw new Error(`${night} maps to both ${palette[night]} and ${day} in light mode`);
    }
    palette[night] = day;
  }
  return palette;
}

const generated = join(here, 'generated');
rmSync(generated, { recursive: true, force: true });
mkdirSync(generated);
writeFileSync(
  join(generated, 'anvil-night.json'),
  JSON.stringify(remapTheme(tokyoNight, 'anvil-night', 'dark', nightRemap)),
);
writeFileSync(join(generated, 'day-palette.json'), JSON.stringify(dayPalette(tokyoNight)));
// One dynamic import per grammar languages.js can pick, so the bundled set
// always matches the page's extension map.
writeFileSync(
  join(generated, 'langs.mjs'),
  `export const LANGS = {\n${LANGUAGES.map(
    (id) => `  ${JSON.stringify(id)}: () => import(${JSON.stringify(`@shikijs/langs/${id}`)}),\n`,
  ).join('')}};\n`,
);

// Build outside the embedded tree and swap it in only on success, so a
// failed build never leaves the UI without its highlighter or embeds a
// half-built copy.
const staging = join(generated, 'vendor');
rmSync(staging, { recursive: true, force: true });
const { metafile } = await build({
  entryPoints: { shiki: join(here, 'entry.mjs') },
  outdir: staging,
  bundle: true,
  splitting: true,
  format: 'esm',
  minify: true,
  target: 'es2022',
  chunkNames: 'chunks/[name]-[hash]',
  legalComments: 'none',
  metafile: true,
  logLevel: 'info',
});
writeFileSync(join(staging, 'THIRD_PARTY_NOTICES.md'), notices(metafile));
rmSync(out, { recursive: true, force: true });
renameSync(staging, out);

// notices lists every npm package, grammar and theme the bundle contains,
// with licence texts for the packages, since minifying strips their headers.
function notices(meta) {
  const packages = new Set();
  const langs = new Set();
  for (const input of Object.keys(meta.inputs)) {
    const m = input.match(/node_modules\/((?:@[^/]+\/)?[^/]+)\//);
    if (m) packages.add(m[1]);
    const lang = input.match(/node_modules\/@shikijs\/langs\/dist\/([^/]+)\.mjs$/);
    if (lang) langs.add(lang[1]);
  }
  const lines = [
    '# Third-party notices',
    '',
    'Generated by tools/shiki/build.mjs. The bundle in this directory contains',
    'the following grammars, theme and npm packages.',
    '',
    '## TextMate grammars',
    '',
  ];
  for (const name of [...langs].sort()) {
    const g = [...grammars, ...injections].find((x) => x.name === name);
    if (!g) throw new Error(`no tm-grammars metadata for bundled grammar ${name}`);
    lines.push(`- ${g.displayName ?? g.name} (${g.name}): ${g.license ?? "licence not stated upstream"}, ${g.source}`);
  }
  const theme = themes.find((t) => t.name === 'tokyo-night');
  lines.push('', '## Theme', '', `- ${theme.displayName}: ${theme.license}, ${theme.source}`);
  lines.push('', '## npm packages');
  for (const pkg of [...packages].sort()) {
    const dir = join(here, 'node_modules', pkg);
    const { version, license } = JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8'));
    lines.push('', `### ${pkg}@${version} (${license})`, '');
    const file = ['LICENSE', 'LICENSE.md', 'LICENSE.txt', 'license'].find((f) => existsSync(join(dir, f)));
    if (!file) throw new Error(`no licence file for bundled package ${pkg}`);
    lines.push('```', readFileSync(join(dir, file), 'utf8').trim(), '```');
  }
  return `${lines.join('\n')}\n`;
}
