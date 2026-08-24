package web

import (
	"bytes"
	"net/http"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// Highlighting caps: past either bound the file API returns an empty
// highlighted field and the UI falls back to a plain <pre> view. Chroma
// tokenizes the whole file in memory, so unbounded input would make the
// worst-case /api/file response CPU- and allocation-heavy.
const (
	maxHighlightBytes = 1 << 20 // 1 MiB
	maxHighlightLines = 10_000
)

// Chroma style names for the generated stylesheet. Both are emitted by
// /chroma.css — the dark one scoped under a prefers-color-scheme media
// query — so the viewer follows the system theme with class-based CSS
// only (no inline styles in the highlighted HTML).
const (
	lightStyleName = "github"
	darkStyleName  = "github-dark"
)

// htmlFormatter emits class-based HTML (WithClasses: no inline styles)
// with a linkable line-number gutter (span ids L1, L2, ...) that the UI
// uses for scroll-to-line and per-line anchors. It is safe for concurrent
// use: Format does not mutate the formatter.
var htmlFormatter = chromahtml.New(
	chromahtml.WithClasses(true),
	chromahtml.WithLineNumbers(true),
	chromahtml.WithLinkableLineNumbers(true, "L"),
)

// highlight returns content rendered as chroma-highlighted HTML, choosing
// the lexer by file name with a plaintext fallback. It returns "" (the
// plain-view fallback signal) for oversized content or on tokenizer
// errors. Chroma escapes all source text, so the result is safe to inject
// into the page by construction.
func highlight(path, content string, totalLines int) string {
	if len(content) > maxHighlightBytes || totalLines > maxHighlightLines {
		return ""
	}
	lexer := lexers.Match(path)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	iterator, err := chroma.Coalesce(lexer).Tokenise(nil, content)
	if err != nil {
		return ""
	}
	var b strings.Builder
	// With classes enabled the style only matters for CSS generation, but
	// Format still requires one; pass the light style for consistency.
	if err := htmlFormatter.Format(&b, styles.Get(lightStyleName), iterator); err != nil {
		return ""
	}
	return b.String()
}

// chromaCSS is generated once on first request: the stylesheet content is
// a pure function of the pinned chroma version and the two style names.
var (
	chromaCSSOnce sync.Once
	chromaCSS     []byte
)

// handleChromaCSS serves GET /chroma.css: chroma's class definitions for
// both themes, each scoped under a prefers-color-scheme media query. Both
// must be scoped — an unscoped light theme would leak rules for token
// classes the dark theme doesn't define (near-black text on a dark page).
func (s *Server) handleChromaCSS(w http.ResponseWriter, _ *http.Request) {
	chromaCSSOnce.Do(func() {
		var b bytes.Buffer
		// WriteCSS only fails on writer errors; bytes.Buffer cannot fail.
		b.WriteString("@media (prefers-color-scheme: light) {\n")
		_ = htmlFormatter.WriteCSS(&b, styles.Get(lightStyleName))
		b.WriteString("}\n@media (prefers-color-scheme: dark) {\n")
		_ = htmlFormatter.WriteCSS(&b, styles.Get(darkStyleName))
		b.WriteString("}\n")
		chromaCSS = b.Bytes()
	})
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write(chromaCSS)
}
