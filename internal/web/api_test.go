package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/broderick-westrope/muninn/internal/index"
	"github.com/broderick-westrope/muninn/internal/search"
	"github.com/broderick-westrope/muninn/internal/status"
)

const widgetGo = `package widget

// Frobnicate does the thing.
func Frobnicate() int { return 42 }

func callerOne() int { return Frobnicate() }

func callerTwo() int { return Frobnicate() }
`

const otherGo = `package widget

const banana = "yellow"
`

// binaryContent contains NUL bytes so the file API's binary guard trips.
const binaryContent = "\x00\x01\x02binary\x00garbage"

// hugeText exceeds the highlighting line cap so the file API must return
// an empty highlighted field (plain-view fallback).
var hugeText = strings.Repeat("data\n", maxHighlightLines+1)

// git runs a git command against dir (or without -C when dir is empty),
// failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{"-c", "user.name=test", "-c", "user.email=test@example.com"}
	if dir != "" {
		base = append(base, "-C", dir)
	}
	cmd := exec.Command("git", append(base, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newFixture builds acme/widget as a bare mirror plus a shard directory
// (indexed without ctags — no test here needs sym: queries) and a fresh
// status file, and returns a Server over them with the indexed commit.
func newFixture(t *testing.T) (*Server, string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	git(t, "", "init", "-b", "main", src)
	files := map[string]string{
		"widget.go": widgetGo,
		"other.go":  otherGo,
		"data.bin":  binaryContent,
		"big.txt":   hugeText,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
			t.Fatalf("writing fixture file %s: %v", name, err)
		}
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "initial")

	root := t.TempDir()
	mirrorsDir := filepath.Join(root, "mirrors")
	mirror := filepath.Join(mirrorsDir, "acme", "widget.git")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("creating mirrors dir: %v", err)
	}
	git(t, "", "clone", "--bare", src, mirror)
	commit := git(t, mirror, "rev-parse", "refs/heads/main")

	indexDir := filepath.Join(root, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatalf("creating index dir: %v", err)
	}
	ix := &index.Indexer{IndexDir: indexDir}
	if err := ix.IndexRepo(context.Background(), mirror, "acme/widget", "main", commit); err != nil {
		t.Fatalf("IndexRepo: %v", err)
	}

	statusPath := filepath.Join(root, "status.json")
	writeStatus(t, statusPath, commit, time.Now())

	searcher, err := search.Open(indexDir)
	if err != nil {
		t.Fatalf("search.Open: %v", err)
	}
	t.Cleanup(searcher.Close)

	return New(searcher, statusPath, mirrorsDir), commit
}

// writeStatus writes a successful sync status for acme/widget finished at
// the given time.
func writeStatus(t *testing.T, path, commit string, finishedAt time.Time) {
	t.Helper()
	err := status.Write(path, &status.SyncStatus{
		StartedAt:  finishedAt.Add(-time.Minute),
		FinishedAt: finishedAt,
		Success:    true,
		Repos: map[string]status.RepoStatus{
			"acme/widget": {Fetched: true, Indexed: true, IndexedCommit: commit},
		},
	})
	if err != nil {
		t.Fatalf("status.Write: %v", err)
	}
}

// get performs a GET request against the server's handler and decodes the
// JSON response body into out (skipped when out is nil).
func get(t *testing.T, srv *Server, target string, out any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET %s: Content-Type = %q, want application/json", target, ct)
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("GET %s: decoding body %q: %v", target, rec.Body.String(), err)
		}
	}
	return rec
}

// errorBody decodes a JSON error response and returns its message.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding error body %q: %v", rec.Body.String(), err)
	}
	return body["error"]
}

func TestAPISearch(t *testing.T) {
	srv, _ := newFixture(t)
	var res searchResponse
	rec := get(t, srv, `/api/search?q=%22banana%22`, &res)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(res.Files) != 1 || res.Files[0].Repo != "acme/widget" || res.Files[0].Path != "other.go" {
		t.Fatalf("files = %+v, want single match in acme/widget other.go", res.Files)
	}
	if got := res.Files[0].Lines[0].LineNumber; got != 3 {
		t.Errorf("line number = %d, want 3", got)
	}
	if res.Truncated {
		t.Error("truncated = true, want false")
	}
	if res.Stats.MatchCount == 0 {
		t.Error("stats.matchCount = 0, want > 0")
	}
}

func TestAPISearchParseError(t *testing.T) {
	srv, _ := newFixture(t)
	rec := get(t, srv, `/api/search?q=%28unbalanced`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); msg == "" {
		t.Error("error message is empty, want the parser message")
	}
}

func TestAPISearchMissingQuery(t *testing.T) {
	srv, _ := newFixture(t)
	rec := get(t, srv, `/api/search`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestAPISearchLimit(t *testing.T) {
	srv, _ := newFixture(t)

	// Frobnicate appears on 4 lines; limit=1 must cap and mark truncation.
	var res searchResponse
	rec := get(t, srv, `/api/search?q=Frobnicate&limit=1`, &res)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	shown := 0
	for _, f := range res.Files {
		shown += len(f.Lines)
	}
	if shown != 1 {
		t.Errorf("returned lines = %d, want 1", shown)
	}
	if !res.Truncated {
		t.Error("truncated = false, want true when the limit cuts results")
	}

	rec = get(t, srv, `/api/search?q=Frobnicate&limit=nope`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-integer limit: status = %d, want 400", rec.Code)
	}
}

func TestParseLimitClamp(t *testing.T) {
	tests := []struct {
		raw  string
		want int
	}{
		{"", defaultSearchLimit},
		{"0", defaultSearchLimit},
		{"-5", defaultSearchLimit},
		{"10", 10},
		{"100000", maxSearchLimit},
	}
	for _, tt := range tests {
		got, err := parseLimit(tt.raw)
		if err != nil {
			t.Errorf("parseLimit(%q): unexpected error %v", tt.raw, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseLimit(%q) = %d, want %d", tt.raw, got, tt.want)
		}
	}
	if _, err := parseLimit("abc"); err == nil {
		t.Error("parseLimit(abc): err = nil, want error")
	}
}

func TestAPIFile(t *testing.T) {
	srv, commit := newFixture(t)
	var res fileResponse
	rec := get(t, srv, `/api/file?repo=acme/widget&path=other.go`, &res)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if res.Content != otherGo {
		t.Errorf("content = %q, want the full file", res.Content)
	}
	if res.TotalLines != 3 {
		t.Errorf("totalLines = %d, want 3", res.TotalLines)
	}
	if res.Language != "go" {
		t.Errorf("language = %q, want go", res.Language)
	}
	if res.IndexedCommit != commit {
		t.Errorf("indexedCommit = %q, want %q", res.IndexedCommit, commit)
	}
}

func TestAPIFileNotFound(t *testing.T) {
	srv, _ := newFixture(t)
	rec := get(t, srv, `/api/file?repo=acme/widget&path=missing.go`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestAPIFileInvalidRepo(t *testing.T) {
	srv, _ := newFixture(t)
	for _, repo := range []string{
		"../widget",
		"./widget",
		"acme/..",
		"acme/.",
		"acme/",
		"/widget",
		"acme/wid/get",
		`acme\evil/widget`,
		"",
	} {
		rec := get(t, srv, "/api/file?repo="+repo+"&path=other.go", nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("repo %q: status = %d, want 400", repo, rec.Code)
		}
	}
}

func TestAPIFileBinary(t *testing.T) {
	srv, _ := newFixture(t)
	rec := get(t, srv, `/api/file?repo=acme/widget&path=data.bin`, nil)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415: %s", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); !strings.Contains(msg, "binary") {
		t.Errorf("error = %q, want a binary-file message", msg)
	}
}

func TestAPIFileHighlighted(t *testing.T) {
	srv, _ := newFixture(t)
	var res fileResponse
	rec := get(t, srv, `/api/file?repo=acme/widget&path=other.go`, &res)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if res.Highlighted == "" {
		t.Fatal("highlighted is empty, want chroma HTML for a .go file")
	}
	if !strings.Contains(res.Highlighted, `class="chroma"`) {
		t.Errorf("highlighted = %q, want class-based chroma markup", res.Highlighted)
	}
	if !strings.Contains(res.Highlighted, `id="L3"`) {
		t.Errorf("highlighted = %q, want linkable line-number anchors (id=\"L3\")", res.Highlighted)
	}
}

func TestAPIFileHighlightCap(t *testing.T) {
	srv, _ := newFixture(t)
	var res fileResponse
	rec := get(t, srv, `/api/file?repo=acme/widget&path=big.txt`, &res)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if res.Highlighted != "" {
		t.Errorf("highlighted is non-empty for %d lines, want plain-view fallback past %d",
			res.TotalLines, maxHighlightLines)
	}
	if res.Content != hugeText {
		t.Error("content missing or wrong, want the full file even when highlighting is skipped")
	}
}

// TestUIStaticAssets exercises the embedded UI routes on a zero Server:
// the static handler and chroma stylesheet need no searcher or fixtures.
func TestUIStaticAssets(t *testing.T) {
	handler := (&Server{}).Handler()
	tests := []struct {
		path     string
		wantType string
		wantBody string
	}{
		{"/", "text/html", "<title>muninn"},
		{"/app.js", "javascript", "runSearch"},
		{"/style.css", "text/css", "prefers-color-scheme"},
		{"/chroma.css", "text/css", "@media (prefers-color-scheme: dark)"},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", tt.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tt.wantType) {
			t.Errorf("GET %s: Content-Type = %q, want it to contain %q", tt.path, ct, tt.wantType)
		}
		if !strings.Contains(rec.Body.String(), tt.wantBody) {
			t.Errorf("GET %s: body does not contain %q", tt.path, tt.wantBody)
		}
	}
}

func TestAPIRepos(t *testing.T) {
	srv, commit := newFixture(t)
	var repos []repoJSON
	rec := get(t, srv, `/api/repos`, &repos)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(repos) != 1 {
		t.Fatalf("repos = %+v, want exactly acme/widget", repos)
	}
	r := repos[0]
	if r.Name != "acme/widget" || r.Branch != "main" {
		t.Errorf("repo = %+v, want acme/widget on main", r)
	}
	if r.ShortCommit != commit[:7] {
		t.Errorf("shortCommit = %q, want %q", r.ShortCommit, commit[:7])
	}
	if r.Stale {
		t.Error("stale = true, want false for a fresh sync")
	}
	if r.IndexAge == "" || r.IndexAge == "unknown" {
		t.Errorf("indexAge = %q, want a duration string", r.IndexAge)
	}
}

func TestAPIReposStale(t *testing.T) {
	srv, commit := newFixture(t)
	writeStatus(t, srv.statusPath, commit, time.Now().Add(-48*time.Hour))
	var repos []repoJSON
	get(t, srv, `/api/repos`, &repos)
	if len(repos) != 1 || !repos[0].Stale {
		t.Errorf("repos = %+v, want acme/widget marked stale after 48h", repos)
	}
}

func TestValidateLoopback(t *testing.T) {
	allowed := []string{
		"127.0.0.1:7576",
		"127.0.0.2:80",
		"[::1]:7576",
		"localhost:0",
	}
	for _, addr := range allowed {
		if err := ValidateLoopback(addr); err != nil {
			t.Errorf("ValidateLoopback(%q) = %v, want nil", addr, err)
		}
	}
	refused := []string{
		"0.0.0.0:7576",
		"[::]:7576",
		"192.168.1.5:7576",
		"10.0.0.1:80",
		":7576",
		"127.0.0.1", // no port
	}
	for _, addr := range refused {
		if err := ValidateLoopback(addr); err == nil {
			t.Errorf("ValidateLoopback(%q) = nil, want error", addr)
		}
	}
}
