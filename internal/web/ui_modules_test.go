package web

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestUIModules runs the UI's node:test suites. The UI ships as plain ES
// modules with no build step, so node is only a test dependency: without
// it the suites are skipped, not failed. The tests live under static/_test,
// which go:embed excludes, so they are never served.
func TestUIModules(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping UI module tests")
	}
	files, err := filepath.Glob(filepath.Join("static", "_test", "*.test.mjs"))
	if err != nil {
		t.Fatalf("glob UI tests: err=%v", err)
	}
	if len(files) == 0 {
		t.Fatal("no UI test files found under static/_test")
	}
	out, err := exec.Command(node, append([]string{"--test"}, files...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node --test failed: err=%v\n%s", err, out)
	}
}

// TestUITestsNotServed guards the claim above: renaming static/_test would
// silently start embedding and serving the test suites.
func TestUITestsNotServed(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_test/palette-match.test.mjs", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /_test/palette-match.test.mjs: status = %d, want 404", rec.Code)
	}
}
