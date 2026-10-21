package web_test

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/broderick-westrope/muninn/internal/web"
)

// TestUIModules runs the UI's node:test suites. The UI ships as plain ES
// modules with no build step, so node is only a test dependency: without
// it, or with a node too old to load them, the suites are skipped, not
// failed. The tests live under static/_test, which go:embed excludes, so
// they are never served.
func TestUIModules(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping UI module tests")
	}
	out, err := exec.CommandContext(t.Context(), node, "--version").Output()
	if err != nil {
		t.Fatalf("node --version: %v", err)
	}
	if version := strings.TrimSpace(string(out)); !detectsModules(version) {
		t.Skipf("node %s cannot load the UI's ES modules (needs 20.19+ or 22.7+); skipping", version)
	}
	files, err := filepath.Glob(filepath.Join("static", "_test", "*.test.mjs"))
	if err != nil {
		t.Fatalf("glob UI tests: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no UI test files found under static/_test")
	}
	args := append([]string{"--test", "--test-timeout=30000"}, files...)
	out, err = exec.CommandContext(t.Context(), node, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}

// detectsModules reports whether a node version ("v22.21.0") loads .js
// files written as ES modules without a package.json "type" field. That
// detection is on by default from 20.19 on the 20 line and from 22.7.
func detectsModules(version string) bool {
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	switch {
	case major == 20:
		return minor >= 19
	case major == 22:
		return minor >= 7
	default:
		return major > 22
	}
}

func TestDetectsModules(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"v18.20.4", false},
		{"v20.18.0", false},
		{"v20.19.0", true},
		{"v21.7.3", false},
		{"v22.6.0", false},
		{"v22.7.0", true},
		{"v22.21.0", true},
		{"v24.1.0", true},
		{"garbage", false},
	}
	for _, tt := range tests {
		if got := detectsModules(tt.version); got != tt.want {
			t.Errorf("detectsModules(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}

// TestUITestsNotServed guards the claim above: renaming static/_test would
// silently start embedding and serving the test suites.
func TestUITestsNotServed(t *testing.T) {
	rec := httptest.NewRecorder()
	(&web.Server{}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_test/palette-match.test.mjs", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /_test/palette-match.test.mjs: status = %d, want 404", rec.Code)
	}
}
