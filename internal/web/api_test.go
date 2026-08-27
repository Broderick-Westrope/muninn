package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// subGo lives in a subdirectory so tree listings have a dir entry to
// return. Its contents are distinct from the other fixture files so search
// assertions stay unambiguous.
const subGo = `package sub

const kiwi = "green"
`

// otherTS and makefile give the fixture extension variety sharing one token,
// so extension facets have something to separate — including the
// no-extension bucket.
const otherTS = `export const kiwi = "green";
`

const makefile = "build:\n\t@echo kiwi\n"

// libGo is the second facet-fixture repo's only file.
const libGo = `package lib

const kiwi = "green"
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
		// A subdirectory so tree listings can exercise dir entries.
		"pkg/sub.go": subGo,
		// Extension variety, sharing the "kiwi" token, so extension facets
		// have something to separate.
		"other.ts": otherTS,
		"Makefile": makefile,
	}
	for name, content := range files {
		path := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating fixture directory for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
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

	return New(searcher, statusPath, mirrorsDir, nil, ""), commit
}

// writeStatus writes a successful sync status for acme/widget finished at
// the given time.
func writeStatus(t *testing.T, path, commit string, finishedAt time.Time) {
	t.Helper()
	writeStatusRepos(t, path, finishedAt, map[string]string{"acme/widget": commit})
}

// writeStatusRepos writes a successful sync status for several repos.
func writeStatusRepos(t *testing.T, path string, finishedAt time.Time, commits map[string]string) {
	t.Helper()
	repos := make(map[string]status.RepoStatus, len(commits))
	for name, commit := range commits {
		repos[name] = status.RepoStatus{Fetched: true, Indexed: true, IndexedCommit: commit}
	}
	err := status.Write(path, &status.SyncStatus{
		StartedAt:  finishedAt.Add(-time.Minute),
		FinishedAt: finishedAt,
		Success:    true,
		Repos:      repos,
	})
	if err != nil {
		t.Fatalf("status.Write: %v", err)
	}
}

// newFacetFixture builds a two-repo index so facet alternations can be
// tested for multi-repo OR and for QuoteMeta escaping: the second repo's
// name contains a regex metacharacter. Kept separate from newFixture so the
// single-repo assertions in TestAPIRepos keep holding.
func newFacetFixture(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	mirrorsDir := filepath.Join(root, "mirrors")
	indexDir := filepath.Join(root, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatalf("creating index dir: %v", err)
	}
	commits := map[string]string{}
	for repo, files := range map[string]map[string]string{
		"acme/widget": {"widget.go": widgetGo, "other.ts": otherTS, "Makefile": makefile},
		// The dot is the escaping test: an unquoted "." would also match
		// "acme/myxlib".
		"acme/my.lib": {"lib.go": libGo},
	} {
		src := filepath.Join(t.TempDir(), "src")
		git(t, "", "init", "-b", "main", src)
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
				t.Fatalf("writing %s: %v", name, err)
			}
		}
		git(t, src, "add", ".")
		git(t, src, "commit", "-m", "initial")

		owner, name, _ := strings.Cut(repo, "/")
		mirror := filepath.Join(mirrorsDir, owner, name+".git")
		if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
			t.Fatalf("creating mirror dir: %v", err)
		}
		git(t, "", "clone", "--bare", src, mirror)
		commit := git(t, mirror, "rev-parse", "refs/heads/main")
		commits[repo] = commit

		ix := &index.Indexer{IndexDir: indexDir}
		if err := ix.IndexRepo(context.Background(), mirror, repo, "main", commit); err != nil {
			t.Fatalf("IndexRepo %s: %v", repo, err)
		}
	}

	statusPath := filepath.Join(root, "status.json")
	writeStatusRepos(t, statusPath, time.Now(), commits)
	searcher, err := search.Open(indexDir)
	if err != nil {
		t.Fatalf("search.Open: %v", err)
	}
	t.Cleanup(searcher.Close)
	return New(searcher, statusPath, mirrorsDir, nil, "")
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

// TestAPIFileTooLarge commits a blob just over gitfile's 10 MiB cap to a
// dedicated fixture (kept out of newFixture so every other test does not
// pay to index it; handleFile touches only the status file and the
// mirror, never the searcher) and expects 413.
func TestAPIFileTooLarge(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	git(t, "", "init", "-b", "main", src)
	huge := bytes.Repeat([]byte("0123456789abcdef"), (10<<20)/16+1) // > 10 MiB
	if err := os.WriteFile(filepath.Join(src, "huge.txt"), huge, 0o644); err != nil {
		t.Fatalf("writing huge fixture file: %v", err)
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "huge")

	root := t.TempDir()
	mirrorsDir := filepath.Join(root, "mirrors")
	mirror := filepath.Join(mirrorsDir, "acme", "widget.git")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("creating mirrors dir: %v", err)
	}
	git(t, "", "clone", "--bare", src, mirror)
	commit := git(t, mirror, "rev-parse", "refs/heads/main")

	statusPath := filepath.Join(root, "status.json")
	writeStatus(t, statusPath, commit, time.Now())

	srv := New(nil, statusPath, mirrorsDir, nil, "")
	rec := get(t, srv, `/api/file?repo=acme/widget&path=huge.txt`, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); !strings.Contains(msg, "too large") {
		t.Errorf("error = %q, want a too-large message", msg)
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
		{"/main.js", "javascript", "initSearch"},
		{"/search.js", "javascript", "runSearch"},
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

func TestAPIFileLocalPath(t *testing.T) {
	srv, _ := newFixture(t)
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "other.go"), []byte(otherGo), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.checkouts = map[string]string{"acme/widget": checkout}
	srv.editorScheme = "cursor"

	var res fileResponse
	rec := get(t, srv, `/api/file?repo=acme/widget&path=other.go`, &res)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if want := filepath.Join(checkout, "other.go"); res.LocalPath != want {
		t.Errorf("localPath = %q, want %q", res.LocalPath, want)
	}
	if res.EditorScheme != "cursor" {
		t.Errorf("editorScheme = %q, want cursor", res.EditorScheme)
	}
}

func TestAPIFileLocalPathMissingFile(t *testing.T) {
	srv, _ := newFixture(t)
	// The checkout exists but does not contain the requested file (e.g.
	// it is on an older commit).
	srv.checkouts = map[string]string{"acme/widget": t.TempDir()}
	srv.editorScheme = "cursor"

	var res fileResponse
	rec := get(t, srv, `/api/file?repo=acme/widget&path=other.go`, &res)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if res.LocalPath != "" || res.EditorScheme != "" {
		t.Errorf("localPath = %q, editorScheme = %q; want both empty when the file is missing locally",
			res.LocalPath, res.EditorScheme)
	}
}

func TestAPIFileLocalPathUnmappedRepo(t *testing.T) {
	srv, _ := newFixture(t)
	var res fileResponse
	rec := get(t, srv, `/api/file?repo=acme/widget&path=other.go`, &res)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if res.LocalPath != "" || res.EditorScheme != "" {
		t.Errorf("localPath = %q, editorScheme = %q; want both empty with no checkout map",
			res.LocalPath, res.EditorScheme)
	}
}

// treeEntryTypes maps a tree response's entries to path -> type.
func treeEntryTypes(entries []treeEntryJSON) map[string]string {
	byPath := make(map[string]string, len(entries))
	for _, e := range entries {
		byPath[e.Path] = e.Type
	}
	return byPath
}

func TestAPITree(t *testing.T) {
	srv, _ := newFixture(t)
	var body treeResponse
	rec := get(t, srv, `/api/tree?repo=acme/widget`, &body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := treeEntryTypes(body.Entries)
	for _, name := range []string{"widget.go", "other.go", "data.bin", "big.txt"} {
		if got[name] != "file" {
			t.Errorf("%s = %q, want file", name, got[name])
		}
	}
	if got["pkg"] != "dir" {
		t.Errorf("pkg = %q, want dir", got["pkg"])
	}
	// Non-recursive: the subdirectory's contents are not in a root listing.
	if _, ok := got["pkg/sub.go"]; ok {
		t.Error("pkg/sub.go listed at root, want excluded")
	}
	if body.Truncated {
		t.Error("truncated = true, want false")
	}
}

func TestAPITreeSubdir(t *testing.T) {
	srv, _ := newFixture(t)
	var body treeResponse
	get(t, srv, `/api/tree?repo=acme/widget&path=pkg`, &body)
	got := treeEntryTypes(body.Entries)
	if len(got) != 1 || got["pkg/sub.go"] != "file" {
		t.Fatalf("entries = %+v, want only pkg/sub.go (repo-relative)", body.Entries)
	}
	// The anchor itself is not a child of itself.
	if _, ok := got["pkg"]; ok {
		t.Error("anchor entry pkg listed, want excluded")
	}
}

func TestAPITreeNotFound(t *testing.T) {
	srv, _ := newFixture(t)
	rec := get(t, srv, `/api/tree?repo=acme/widget&path=nope`, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rec.Code, errorBody(t, rec))
	}
}

func TestAPITreeNotDir(t *testing.T) {
	srv, _ := newFixture(t)
	rec := get(t, srv, `/api/tree?repo=acme/widget&path=widget.go`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rec.Code, errorBody(t, rec))
	}
}

func TestAPITreeInvalidPath(t *testing.T) {
	srv, _ := newFixture(t)
	for _, path := range []string{"../etc", "pkg/../..", ".", ":(exclude)pkg"} {
		rec := get(t, srv, `/api/tree?repo=acme/widget&path=`+url.QueryEscape(path), nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("path %q: status = %d, want 400", path, rec.Code)
		}
	}
}

func TestAPITreeInvalidRepo(t *testing.T) {
	srv, _ := newFixture(t)
	for _, repo := range []string{"", "widget", "acme/widget/extra", "../acme/widget"} {
		rec := get(t, srv, `/api/tree?repo=`+url.QueryEscape(repo), nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("repo %q: status = %d, want 400", repo, rec.Code)
		}
	}
}

func TestAPITreeUnknownRepo(t *testing.T) {
	srv, _ := newFixture(t)
	rec := get(t, srv, `/api/tree?repo=acme/absent`, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rec.Code, errorBody(t, rec))
	}
}

func TestAPITreeIndexMismatch(t *testing.T) {
	srv, _ := newFixture(t)
	// Point the status file at a commit the mirror does not have.
	writeStatus(t, srv.statusPath, "0000000000000000000000000000000000000000", time.Now())
	rec := get(t, srv, `/api/tree?repo=acme/widget`, nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409: %s", rec.Code, errorBody(t, rec))
	}
}

func TestAPITreeTruncated(t *testing.T) {
	srv, _ := newFixture(t)
	srv.maxTreeEntries = 2
	var body treeResponse
	get(t, srv, `/api/tree?repo=acme/widget`, &body)
	if len(body.Entries) != 2 || !body.Truncated {
		t.Errorf("entries = %d, truncated = %v; want 2 and true", len(body.Entries), body.Truncated)
	}
}

// facetCounts maps a facet slice to value -> count.
func facetCounts(vs []facetValueJSON) map[string]int {
	m := make(map[string]int, len(vs))
	for _, v := range vs {
		m[v.Value] = v.Count
	}
	return m
}

// searchPaths lists "repo/path" for every file in a search response.
func searchPaths(body searchResponse) []string {
	var out []string
	for _, f := range body.Files {
		out = append(out, f.Repo+"/"+f.Path)
	}
	return out
}

func TestAPIFacets(t *testing.T) {
	srv := newFacetFixture(t)
	var body facetsJSON
	get(t, srv, `/api/facets?q=kiwi`, &body)

	repos := facetCounts(body.Repos)
	if repos["acme/widget"] == 0 || repos["acme/my.lib"] == 0 {
		t.Errorf("repo facets = %+v, want both repos", body.Repos)
	}
	exts := facetCounts(body.Exts)
	for _, want := range []string{"go", "ts", ""} {
		if exts[want] == 0 {
			t.Errorf("ext facets = %+v, want a bucket for %q", body.Exts, want)
		}
	}
	if body.Partial {
		t.Error("partial = true for a fixture that fits inside the deadline")
	}
}

func TestAPIFacetsMissingQuery(t *testing.T) {
	srv := newFacetFixture(t)
	rec := get(t, srv, `/api/facets`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestAPIFacetsParseError(t *testing.T) {
	srv := newFacetFixture(t)
	rec := get(t, srv, `/api/facets?q=`+url.QueryEscape("repo:["), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestAPIFacetsIgnoreFacetParams pins that the endpoint ignores a facet
// selection: the sidebar's value list must not shrink as the user clicks.
func TestAPIFacetsIgnoreFacetParams(t *testing.T) {
	srv := newFacetFixture(t)
	var plain, filtered facetsJSON
	get(t, srv, `/api/facets?q=kiwi`, &plain)
	get(t, srv, `/api/facets?q=kiwi&repo=`+url.QueryEscape("acme/widget")+`&ext=go`, &filtered)

	if len(plain.Repos) != len(filtered.Repos) || len(plain.Exts) != len(filtered.Exts) {
		t.Errorf("facets changed with facet params: %+v then %+v", plain, filtered)
	}
}

// TestAPISearchOmitsFacets pins the split: aggregation is exhaustive and
// therefore far slower than the capped results pass, so it must not ride
// along on every keystroke's search.
func TestAPISearchOmitsFacets(t *testing.T) {
	srv := newFacetFixture(t)
	rec := get(t, srv, `/api/search?q=kiwi`, nil)
	if body := rec.Body.String(); strings.Contains(body, `"facets"`) {
		t.Errorf("search response still carries a facets block: %s", body)
	}
}

// TestAPIFacetCountNotBelowRows pins that an exact aggregated count is never
// lower than the rows a capped search displays for that value.
func TestAPIFacetCountNotBelowRows(t *testing.T) {
	srv := newFacetFixture(t)
	var results searchResponse
	var facets facetsJSON
	get(t, srv, `/api/search?q=Frobnicate&repo=`+url.QueryEscape("acme/widget"), &results)
	get(t, srv, `/api/facets?q=Frobnicate`, &facets)

	rows := 0
	for _, f := range results.Files {
		if n := len(f.Lines); n > 0 {
			rows += n
		} else {
			rows++
		}
	}
	if got := facetCounts(facets.Repos)["acme/widget"]; got < rows {
		t.Errorf("facet count %d is below the %d rows displayed for it", got, rows)
	}
}

func TestAPISearchFacetFilter(t *testing.T) {
	srv := newFacetFixture(t)
	var body searchResponse
	get(t, srv, `/api/search?q=kiwi&repo=`+url.QueryEscape("acme/my.lib"), &body)
	if got := searchPaths(body); len(got) != 1 || got[0] != "acme/my.lib/lib.go" {
		t.Errorf("files = %v, want only acme/my.lib/lib.go", got)
	}

	var none searchResponse
	get(t, srv, `/api/search?q=kiwi&repo=`+url.QueryEscape("acme/absent"), &none)
	if len(none.Files) != 0 {
		t.Errorf("files = %v, want none for an unmatched repo", searchPaths(none))
	}
}

// TestAPISearchFacetTwoRepos covers OR within a category: two selected repos
// must both come back, which a naive ANDed filter would make impossible.
func TestAPISearchFacetTwoRepos(t *testing.T) {
	srv := newFacetFixture(t)
	var body searchResponse
	get(t, srv, `/api/search?q=kiwi&repo=`+url.QueryEscape("acme/widget,acme/my.lib"), &body)

	seen := map[string]bool{}
	for _, f := range body.Files {
		seen[f.Repo] = true
	}
	if !seen["acme/widget"] || !seen["acme/my.lib"] {
		t.Errorf("repos = %v, want files from both selected repos", seen)
	}
}

// TestAPISearchFacetIntersection covers AND across categories.
func TestAPISearchFacetIntersection(t *testing.T) {
	srv := newFacetFixture(t)
	var body searchResponse
	get(t, srv, `/api/search?q=kiwi&repo=`+url.QueryEscape("acme/widget")+`&ext=ts`, &body)
	if got := searchPaths(body); len(got) != 1 || got[0] != "acme/widget/other.ts" {
		t.Errorf("files = %v, want only acme/widget/other.ts", got)
	}
}

// TestAPISearchFacetRegexMeta pins that facet values are quoted: the "." in
// "acme/my.lib" must be literal, not a wildcard.
func TestAPISearchFacetRegexMeta(t *testing.T) {
	srv := newFacetFixture(t)
	if got := facetRepoFilter([]string{"acme/my.lib"}); got != `(^acme/my\.lib$)` {
		t.Errorf("facetRepoFilter = %q, want the dot escaped", got)
	}
	var body searchResponse
	get(t, srv, `/api/search?q=kiwi&repo=`+url.QueryEscape("acme/myxlib"), &body)
	if len(body.Files) != 0 {
		t.Errorf("files = %v, want none: 'acme/myxlib' must not match 'acme/my.lib'", searchPaths(body))
	}
}

func TestAPISearchFacetExtFilter(t *testing.T) {
	srv := newFacetFixture(t)
	tests := []struct {
		ext  string
		want []string
	}{
		{"ts", []string{"acme/widget/other.ts"}},
		{"", []string{"acme/widget/Makefile"}},
	}
	for _, tt := range tests {
		var body searchResponse
		get(t, srv, `/api/search?q=kiwi&repo=`+url.QueryEscape("acme/widget")+`&ext=`+tt.ext, &body)
		got := searchPaths(body)
		if len(got) != len(tt.want) || (len(got) > 0 && got[0] != tt.want[0]) {
			t.Errorf("ext=%q: files = %v, want %v", tt.ext, got, tt.want)
		}
	}
}

// TestAPISearchFacetWithTypedAtom checks a hand-typed repo: atom composes
// with a facet param rather than conflicting with it.
func TestAPISearchFacetWithTypedAtom(t *testing.T) {
	srv := newFacetFixture(t)
	var body searchResponse
	get(t, srv, `/api/search?q=`+url.QueryEscape("kiwi repo:widget")+`&ext=ts`, &body)
	if got := searchPaths(body); len(got) != 1 || got[0] != "acme/widget/other.ts" {
		t.Errorf("files = %v, want the typed atom and the facet to both apply", got)
	}
}

func TestAPISearchFacetCaps(t *testing.T) {
	srv := newFacetFixture(t)
	many := func(n int, prefix string) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf("%s%d", prefix, i)
		}
		return strings.Join(parts, ",")
	}
	tests := []struct{ name, param string }{
		{"repos", `repo=` + url.QueryEscape(many(maxFacetRepos+1, "acme/r"))},
		{"exts", `ext=` + url.QueryEscape(many(maxFacetExts+1, "e"))},
	}
	for _, tt := range tests {
		rec := get(t, srv, `/api/search?q=kiwi&`+tt.param, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s over cap: status = %d, want 400", tt.name, rec.Code)
		}
	}
}

func TestAPISearchFacetExtValidation(t *testing.T) {
	srv := newFacetFixture(t)
	for _, ext := range []string{`go$|.*`, `.go`, `a/b`, `(`} {
		rec := get(t, srv, `/api/search?q=kiwi&ext=`+url.QueryEscape(ext), nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("ext %q: status = %d, want 400", ext, rec.Code)
		}
	}
}

// launchCall records one editor launch for the injected launcher.
type launchCall struct {
	scheme, dir, file string
	line              int
}

// newOpenFixture returns a server wired to a real local checkout plus a
// recorder standing in for the editor launch, so the success path is
// testable without executing an editor.
func newOpenFixture(t *testing.T) (*Server, *[]launchCall, string) {
	t.Helper()
	srv, _ := newFixture(t)
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "widget.go"), []byte(widgetGo), 0o644); err != nil {
		t.Fatalf("writing checkout file: %v", err)
	}
	srv.checkouts = map[string]string{"acme/widget": checkout}
	srv.editorScheme = "cursor"

	var calls []launchCall
	srv.launch = func(scheme, dir, file string, line int) error {
		calls = append(calls, launchCall{scheme, dir, file, line})
		return nil
	}
	return srv, &calls, checkout
}

// postOpen sends a POST /api/open with same-origin JSON headers by default;
// headers overrides individual values (an empty value removes the header).
func postOpen(t *testing.T, srv *Server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/open", strings.NewReader(body))
	set := map[string]string{"Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"}
	for k, v := range headers {
		set[k] = v
	}
	for k, v := range set {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAPIOpenSuccess(t *testing.T) {
	srv, calls, checkout := newOpenFixture(t)
	resolved, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	rec := postOpen(t, srv, `{"repo":"acme/widget","path":"widget.go","line":7}`, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("launches = %d, want 1", len(*calls))
	}
	got := (*calls)[0]
	// The folder argument must be the checkout root itself — not its parent,
	// which would open the wrong workspace.
	if got.dir != resolved {
		t.Errorf("dir = %q, want the checkout root %q", got.dir, resolved)
	}
	if want := filepath.Join(resolved, "widget.go"); got.file != want {
		t.Errorf("file = %q, want %q", got.file, want)
	}
	if got.line != 7 || got.scheme != "cursor" {
		t.Errorf("launch = %+v, want line 7 and scheme cursor", got)
	}
}

func TestAPIOpenCSRF(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
	}{
		// Absent is rejected, not trusted: otherwise any client omitting the
		// header bypasses the guard.
		{"missing Sec-Fetch-Site", map[string]string{"Sec-Fetch-Site": ""}},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"same-site", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"form content type", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}},
		{"no content type", map[string]string{"Content-Type": ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, calls, _ := newOpenFixture(t)
			rec := postOpen(t, srv, `{"repo":"acme/widget","path":"widget.go","line":1}`, tt.headers)
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			if len(*calls) != 0 {
				t.Error("editor launched despite a rejected request")
			}
		})
	}
}

func TestAPIOpenBadRequest(t *testing.T) {
	tests := []struct{ name, body string }{
		{"malformed", `{`},
		{"zero line", `{"repo":"acme/widget","path":"widget.go","line":0}`},
		{"negative line", `{"repo":"acme/widget","path":"widget.go","line":-3}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, calls, _ := newOpenFixture(t)
			rec := postOpen(t, srv, tt.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if len(*calls) != 0 {
				t.Error("editor launched for an invalid request")
			}
		})
	}
}

func TestAPIOpenUnknownRepo(t *testing.T) {
	srv, _, _ := newOpenFixture(t)
	rec := postOpen(t, srv, `{"repo":"acme/absent","path":"widget.go","line":1}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rec.Code, errorBody(t, rec))
	}
}

func TestAPIOpenMissingFile(t *testing.T) {
	srv, _, _ := newOpenFixture(t)
	// Routine: the checkout is often on a different commit than the index.
	rec := postOpen(t, srv, `{"repo":"acme/widget","path":"absent.go","line":1}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rec.Code, errorBody(t, rec))
	}
}

func TestAPIOpenEscape(t *testing.T) {
	srv, calls, checkout := newOpenFixture(t)
	outside := filepath.Join(filepath.Dir(checkout), "outside.go")
	if err := os.WriteFile(outside, []byte("package outside\n"), 0o644); err != nil {
		t.Fatalf("writing outside file: %v", err)
	}

	rec := postOpen(t, srv, `{"repo":"acme/widget","path":"../outside.go","line":1}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: %s", rec.Code, errorBody(t, rec))
	}
	if len(*calls) != 0 {
		t.Error("editor launched for a path outside the checkout")
	}
}

func TestAPIOpenColonPath(t *testing.T) {
	srv, calls, checkout := newOpenFixture(t)
	// --goto splits its value on ":", so such a path would open the wrong
	// file; the endpoint declines and the client falls back.
	if err := os.WriteFile(filepath.Join(checkout, "od:d.go"), []byte("package odd\n"), 0o644); err != nil {
		t.Fatalf("writing colon file: %v", err)
	}

	rec := postOpen(t, srv, `{"repo":"acme/widget","path":"od:d.go","line":1}`, nil)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501: %s", rec.Code, errorBody(t, rec))
	}
	if len(*calls) != 0 {
		t.Error("editor launched with a colon in the path")
	}
}

func TestAPIOpenCLIMissing(t *testing.T) {
	srv, _, _ := newOpenFixture(t)
	srv.launch = func(string, string, string, int) error {
		return fmt.Errorf("cursor: %w", ErrEditorCLINotFound)
	}
	rec := postOpen(t, srv, `{"repo":"acme/widget","path":"widget.go","line":1}`, nil)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501: %s", rec.Code, errorBody(t, rec))
	}
}

func TestAPIOpenExecFailed(t *testing.T) {
	srv, _, _ := newOpenFixture(t)
	srv.launch = func(string, string, string, int) error {
		return errors.New("fork/exec: resource temporarily unavailable")
	}
	rec := postOpen(t, srv, `{"repo":"acme/widget","path":"widget.go","line":1}`, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: %s", rec.Code, errorBody(t, rec))
	}
}
