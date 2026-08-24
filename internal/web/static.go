package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// staticFiles holds the embedded single-page UI (index.html, app.js,
// style.css). Everything is embedded so the binary works offline — no
// CDN, no remote fonts or icons, no Node build step.
//
//go:embed static
var staticFiles embed.FS

// staticHandler serves the embedded UI with the static/ prefix stripped,
// so GET / serves index.html and /app.js, /style.css serve the assets
// (http.FileServer maps the directory root to index.html).
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		// The embedded tree always contains static/; reaching this is a
		// build defect, not a runtime condition.
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}
