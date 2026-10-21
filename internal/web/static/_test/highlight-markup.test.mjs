import assert from 'node:assert/strict';
import { test } from 'node:test';
import { TOKEN_CLASS, TOKEN_COLOR, highlightFile } from '../highlight-markup.js';
import { DAY_PALETTE, LANGS, THEME, getHighlighter } from '../vendor/shiki/shiki.js';
import { LANGUAGES } from '../languages.js';

// render loads lang's grammar the way highlight-worker.js does and renders
// content through the same highlightFile call.
async function render(lang, content) {
  const hl = await getHighlighter();
  if (!hl.getLoadedLanguages().includes(lang)) await hl.loadLanguage(await LANGS[lang]());
  return highlightFile(hl, { lang, theme: THEME, dayPalette: DAY_PALETTE }, content);
}

// plainLines is how many gutter lines file.js's plainPre renders.
function plainLines(content) {
  const lines = content.split('\n');
  if (lines[lines.length - 1] === '') lines.pop();
  return lines.length;
}

test('the vendored bundle has exactly the grammars languages.js picks', () => {
  assert.deepEqual(Object.keys(LANGS).sort(), LANGUAGES);
});

test('every bundled grammar loads and tokenizes', async () => {
  for (const lang of LANGUAGES) {
    const { html } = await render(lang, 'a = "b" // c\n');
    assert.match(html, /<span class="line">/, lang);
  }
});

test('escapes source text', async () => {
  const { html } = await render('javascript', 'const s = "<script>alert(1)</script>" && a < b & c;\n');
  assert.doesNotMatch(html, /<script>/);
  assert.match(html, /&lt;script&gt;/);
  assert.match(html, />&amp;</);
  assert.match(html, /&quot;/);
  const text = html.replace(/<[^>]*>/g, '');
  assert.doesNotMatch(text, /[<>"]|&(?!amp;|lt;|gt;|quot;)/);
});

test('every class in the HTML is a declared, well-formed style', async () => {
  const { html, styles } = await render('go', 'package x\n\n// c\nfunc F() string { return "s" }\n');
  const declared = new Set(styles.map(([cls]) => cls));
  const used = [...html.matchAll(/<span class="(sk-[^"]*)"/g)].map((m) => m[1]);
  assert.ok(used.length > 0);
  for (const cls of used) {
    assert.match(cls, TOKEN_CLASS);
    assert.ok(declared.has(cls), `${cls} used but not declared`);
  }
  for (const [, dark, light] of styles) {
    assert.match(dark, TOKEN_COLOR);
    assert.match(light, TOKEN_COLOR);
  }
});

test('escapes text in tokens that carry no colour', () => {
  const hl = {
    codeToTokens: () => ({ tokens: [[{ content: '<b a="1">&' }]] }),
  };
  const { html, styles } = highlightFile(hl, { lang: 'x', theme: 't', dayPalette: {} }, 'ignored');
  assert.match(html, /&lt;b a=&quot;1&quot;&gt;&amp;/);
  assert.deepEqual(styles, []);
});

test('lines past the length cap render uncoloured', async () => {
  const long = `const a = "${'x'.repeat(25_000)}";`;
  const { html } = await render('javascript', `${long}\nconst b = 1;\n`);
  const [first, second] = html.split('<span class="line">').slice(1);
  assert.ok(first.includes('x'.repeat(25_000)));
  assert.doesNotMatch(first, /<span class="sk-/);
  assert.match(second, /<span class="sk-/);
});

test('gutter lines match the plain view', async () => {
  for (const content of ['a', 'a\n', 'a\n\n', '\n', 'a\r\nb\r\n', 'a\n\nb']) {
    const { html } = await render('go', content);
    const lines = html.match(/<span class="line">/g)?.length ?? 0;
    assert.equal(lines, plainLines(content), JSON.stringify(content));
    assert.match(
      html,
      new RegExp(`<span class="line"><span class="ln" id="L${lines}"><a class="lnlinks" href="#L${lines}">${lines}</a></span><span class="cl">`),
    );
  }
});

test('light mode uses the Day palette', async () => {
  const { styles } = await render('go', 'package x\n');
  const keyword = styles.find(([, dark]) => dark.toLowerCase() === '#bb9af7');
  assert.ok(keyword, 'package keyword is #bb9af7 in anvil-night');
  assert.equal(keyword[2].toLowerCase(), '#9854f1');
});
