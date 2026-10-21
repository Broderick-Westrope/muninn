package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// staticFiles holds the embedded single-page UI (index.html, the ES
// modules rooted at main.js, and style.css). Everything is embedded so the
// binary works offline — no CDN, no remote fonts or icons, no Node build
// step: the browser loads the modules natively.
//
//go:embed static
var staticFiles embed.FS

// staticHandler serves the embedded UI with the static/ prefix stripped,
// so GET / serves index.html and /main.js, /style.css serve the assets
// (http.FileServer maps the directory root to index.html).
//
// Embedded files carry no modification time, so without Cache-Control a
// browser may keep serving modules from an older muninn after an upgrade,
// mixing old and new code. no-cache makes it revalidate on every load,
// which costs nothing against a loopback server.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		// The embedded tree always contains static/; reaching this is a
		// build defect, not a runtime condition.
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
