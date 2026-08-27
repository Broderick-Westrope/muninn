package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/broderick-westrope/muninn/internal/gitfile"
	"github.com/broderick-westrope/muninn/internal/search"
	"github.com/broderick-westrope/muninn/internal/status"
)

const (
	// defaultSearchLimit is the line-match cap when the limit param is
	// absent or non-positive; maxSearchLimit is the hard maximum.
	defaultSearchLimit = 50
	maxSearchLimit     = 200
	// binarySniffBytes is how much of a file's head is scanned for NUL
	// bytes to reject binary content.
	binarySniffBytes = 8 << 10 // 8 KiB
	// staleAfter is how old the last successful sync may be before repos
	// are reported stale.
	staleAfter = 24 * time.Hour
	// maxFacetRepos and maxFacetExts bound the facet params: every value
	// becomes a branch of a regex alternation compiled on each search.
	maxFacetRepos = 50
	maxFacetExts  = 20
	// facetAggregateLimit caps the facet-free aggregation pass. Well above
	// the display limit so a facet count is never lower than the rows a
	// filtered search returns for that value.
	facetAggregateLimit = 1000
)

// searchResponse is the JSON shape of /api/search.
type searchResponse struct {
	Files     []fileMatchesJSON `json:"files"`
	Truncated bool              `json:"truncated"`
	Stats     statsJSON         `json:"stats"`
	Facets    facetsJSON        `json:"facets"`
}

// facetsJSON is the sidebar's facet universe: repo and extension buckets
// aggregated from the query with no facet filters applied.
type facetsJSON struct {
	Repos []facetValueJSON `json:"repos"`
	Exts  []facetValueJSON `json:"exts"`
	// Truncated reports that the aggregation cap was hit, so the value
	// list is not exhaustive. Distinct from the results-level Truncated.
	Truncated bool `json:"truncated"`
}

// facetValueJSON is one facet bucket. Value is "" in Exts for files with no
// extension.
type facetValueJSON struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// fileMatchesJSON is one matching file in a search response.
type fileMatchesJSON struct {
	Repo  string          `json:"repo"`
	Path  string          `json:"path"`
	Lines []lineMatchJSON `json:"lines"`
}

// lineMatchJSON is one matching line in a search response.
type lineMatchJSON struct {
	LineNumber  int    `json:"lineNumber"`
	Line        string `json:"line"`
	IsSymbolDef bool   `json:"isSymbolDef"`
}

// statsJSON summarizes the work zoekt did for a search.
type statsJSON struct {
	FilesConsidered int   `json:"filesConsidered"`
	MatchCount      int   `json:"matchCount"`
	DurationMs      int64 `json:"durationMs"`
}

// fileResponse is the JSON shape of /api/file.
type fileResponse struct {
	Content string `json:"content"`
	// Highlighted is the whole file as chroma-generated HTML (class-based,
	// escaped by construction). It is empty when the file exceeds the
	// highlighting caps, signalling the UI to fall back to a plain view.
	Highlighted   string `json:"highlighted"`
	IndexedCommit string `json:"indexedCommit"`
	TotalLines    int    `json:"totalLines"`
	LocalPath     string `json:"localPath,omitempty"`    // abs path in a local checkout, when found
	EditorScheme  string `json:"editorScheme,omitempty"` // "cursor" or "vscode", set when LocalPath is
}

// repoJSON is one entry of /api/repos.
type repoJSON struct {
	Name        string `json:"name"`
	Branch      string `json:"branch"`
	ShortCommit string `json:"shortCommit"`
	IndexAge    string `json:"indexAge"`
	Stale       bool   `json:"stale"`
}

// treeResponse is the JSON shape of /api/tree.
type treeResponse struct {
	Entries   []treeEntryJSON `json:"entries"`
	Truncated bool            `json:"truncated"`
}

// treeEntryJSON is one child of a listed directory.
type treeEntryJSON struct {
	Path string `json:"path"` // repo-relative
	Type string `json:"type"` // "file" or "dir"
	Size int64  `json:"size"` // 0 for directories
}

// handleSearch serves GET /api/search?q=<query>&limit=<n>&repo=<a,b>&ext=<go,ts>.
// Search errors are reported as 400 with the parser message so the UI can
// show them inline; the query is the only free-form input, so failures are
// user-attributable.
//
// The repo and ext facet params filter the results but deliberately do not
// touch the facet block, which is aggregated from the same query with no
// facet filters. Deriving facet values from the filtered results would make
// every unselected value vanish on the first click.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "missing query parameter q")
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	query := r.URL.Query()
	repos, err := parseFacetParam(query, "repo", maxFacetRepos)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	exts, err := parseFacetParam(query, "ext", maxFacetExts)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, ext := range exts {
		if !validExt(ext) {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("invalid ext %q: expected a bare file extension", ext))
			return
		}
	}

	// Fetch one line past the cap so truncation is detected even when
	// zoekt's own display limits would round up.
	res, err := s.searcher.Search(r.Context(), search.Options{
		Query:      q,
		RepoFilter: facetRepoFilter(repos),
		FileFilter: facetExtFilter(exts),
		MaxResults: limit + 1,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Always a separate pass, never reusing res: it runs at a lower cap, so
	// counts taken from it would change the moment a facet is selected —
	// exactly the instability this design exists to avoid.
	facets, err := s.searcher.Aggregate(r.Context(), q, facetAggregateLimit)
	if err != nil {
		// The query already parsed for the results pass, so a failure here
		// is a server fault rather than bad input.
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := trimResult(res, limit)
	out.Facets = toFacetsJSON(facets)
	writeJSON(w, http.StatusOK, out)
}

// parseFacetParam splits a comma-separated facet param into its values,
// rejecting more than max: each value becomes a branch of a regex
// alternation compiled on every search.
//
// Presence is checked rather than emptiness, because "" is a meaningful
// value — it is the no-extension bucket — so "?ext=" selects extensionless
// files while omitting the param entirely applies no filter.
func parseFacetParam(query url.Values, name string, max int) ([]string, error) {
	if !query.Has(name) {
		return nil, nil
	}
	parts := strings.Split(query.Get(name), ",")
	values := make([]string, len(parts))
	for i, p := range parts {
		values[i] = strings.TrimSpace(p)
	}
	if len(values) > max {
		return nil, fmt.Errorf("too many %s values: %d exceeds the limit of %d", name, len(values), max)
	}
	return values, nil
}

// validExt reports whether an extension facet value is a bare extension.
// Empty is legal: it is the no-extension bucket.
func validExt(ext string) bool {
	for _, r := range ext {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '+':
		default:
			return false
		}
	}
	return true
}

// facetRepoFilter builds an anchored alternation over exact repo names.
// Values are quoted: a repo named "acme/my.lib" would otherwise match
// characters it should not.
func facetRepoFilter(repos []string) string {
	if len(repos) == 0 {
		return ""
	}
	alts := make([]string, len(repos))
	for i, repo := range repos {
		alts[i] = "^" + regexp.QuoteMeta(repo) + "$"
	}
	return "(" + strings.Join(alts, "|") + ")"
}

// facetExtFilter builds an alternation over file extensions. The empty value
// means "no extension": a basename whose only dot, if any, is a leading one,
// so both "Makefile" and ".gitignore" qualify — matching how search.extOf
// buckets them.
func facetExtFilter(exts []string) string {
	if len(exts) == 0 {
		return ""
	}
	alts := make([]string, len(exts))
	for i, ext := range exts {
		if ext == "" {
			alts[i] = `(^|/)\.?[^/.]+$`
			continue
		}
		alts[i] = `\.` + regexp.QuoteMeta(ext) + `$`
	}
	return "(" + strings.Join(alts, "|") + ")"
}

// toFacetsJSON maps aggregated facets to the response shape.
func toFacetsJSON(f *search.Facets) facetsJSON {
	out := facetsJSON{
		Repos:     make([]facetValueJSON, 0, len(f.Repos)),
		Exts:      make([]facetValueJSON, 0, len(f.Exts)),
		Truncated: f.Truncated,
	}
	for _, v := range f.Repos {
		out.Repos = append(out.Repos, facetValueJSON{Value: v.Value, Count: v.Count})
	}
	for _, v := range f.Exts {
		out.Exts = append(out.Exts, facetValueJSON{Value: v.Value, Count: v.Count})
	}
	return out
}

// handleFile serves GET /api/file?repo=<owner/name>&path=<p>, returning
// the whole file pinned to the repo's indexed commit (the viewer needs
// complete content for scroll-to-line).
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		writeError(w, http.StatusBadRequest, "missing query parameter path")
		return
	}
	mirrorDir, commit, herr := s.resolveIndexedCommit(repo)
	if herr != nil {
		writeError(w, herr.code, herr.msg)
		return
	}

	content, totalLines, err := gitfile.ReadFile(r.Context(), mirrorDir, commit, filePath, 0, 0)
	switch {
	case errors.Is(err, gitfile.ErrFileTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	case errors.Is(err, gitfile.ErrIndexMismatch):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if isBinary(content) {
		writeError(w, http.StatusUnsupportedMediaType,
			fmt.Sprintf("%q looks like a binary file and cannot be displayed", filePath))
		return
	}
	localPath, scheme := s.localFile(repo, filePath)

	writeJSON(w, http.StatusOK, fileResponse{
		Content:       content,
		Highlighted:   highlight(filePath, content, totalLines),
		IndexedCommit: commit,
		TotalLines:    totalLines,
		LocalPath:     localPath,
		EditorScheme:  scheme,
	})
}

// localFile resolves a repo-relative path to an absolute path inside a
// scanned local checkout, returning it with the editor scheme when the
// file exists on disk. The checkout may be on a different commit than
// the index, so line numbers derived from indexed content are
// best-effort; the UI discloses this to the user.
func (s *Server) localFile(repo, filePath string) (localPath, scheme string) {
	checkout, ok := s.checkouts[strings.ToLower(repo)]
	if !ok {
		return "", ""
	}
	abs := filepath.Join(checkout, filepath.FromSlash(filePath))
	if info, err := os.Stat(abs); err != nil || info.IsDir() {
		return "", ""
	}
	return abs, s.editorScheme
}

// handleTree serves GET /api/tree?repo=<owner/name>&path=<dir>, listing one
// directory's immediate children pinned to the repo's indexed commit.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	dir := r.URL.Query().Get("path") // "" lists the repo root
	if !validTreePath(dir) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid path %q", dir))
		return
	}
	mirrorDir, commit, herr := s.resolveIndexedCommit(repo)
	if herr != nil {
		writeError(w, herr.code, herr.msg)
		return
	}

	entries, total, err := gitfile.ListDir(r.Context(), mirrorDir, commit, dir, s.maxTreeEntries)
	switch {
	case errors.Is(err, gitfile.ErrNotDir):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, gitfile.ErrIndexMismatch):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := treeResponse{Entries: make([]treeEntryJSON, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, treeEntryJSON{Path: e.Path, Type: e.Type, Size: e.Size})
	}
	out.Truncated = total > len(entries)
	writeJSON(w, http.StatusOK, out)
}

// validTreePath rejects tree paths reaching a dot segment or git pathspec
// magic, which stays active even after "--". This is not a filesystem escape
// (the path addresses a tree object, not disk), but ".." deserves a 400
// rather than a git error surfacing as a 500.
func validTreePath(p string) bool {
	if strings.HasPrefix(p, ":") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// handleRepos serves GET /api/repos: every indexed repo with its branch,
// short indexed commit, human-readable index age, and staleness.
func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.searcher.ListRepos(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// One status read feeds both the age string and the stale flag so
	// they can never disagree. A missing status file means never synced:
	// age unknown, stale.
	indexAge, stale := "unknown", true
	if st, err := status.Read(s.statusPath); err == nil {
		age := status.Age(st)
		indexAge = formatAge(age)
		stale = age > staleAfter
	}

	out := make([]repoJSON, 0, len(repos))
	for _, repo := range repos {
		out = append(out, repoJSON{
			Name:        repo.Name,
			Branch:      repo.Branch,
			ShortCommit: shortSHA(repo.IndexedCommit),
			IndexAge:    indexAge,
			Stale:       stale,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// httpError carries an HTTP status code alongside a message.
type httpError struct {
	code int
	msg  string
}

// resolveIndexedCommit re-reads the status file and returns the mirror
// directory and indexed commit for an exact owner/name repo. The status
// file is never cached: a mid-session sync must be picked up immediately
// so file reads stay pinned to the commit the shards were built from.
// (Deliberately duplicates the MCP server's resolution rather than
// importing it; the packages are independent frontends.)
func (s *Server) resolveIndexedCommit(repo string) (mirrorDir, commit string, herr *httpError) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !validRepoPart(owner) || !validRepoPart(name) {
		return "", "", &httpError{http.StatusBadRequest,
			fmt.Sprintf("invalid repo %q: must be an exact owner/name", repo)}
	}
	st, err := status.Read(s.statusPath)
	if err != nil {
		if errors.Is(err, status.ErrNotExist) {
			return "", "", &httpError{http.StatusNotFound,
				"no sync status found (never synced?); run `muninn sync`"}
		}
		return "", "", &httpError{http.StatusInternalServerError, err.Error()}
	}
	rs, ok := st.Repos[repo]
	if !ok || rs.IndexedCommit == "" {
		return "", "", &httpError{http.StatusNotFound,
			fmt.Sprintf("repo %q has no indexed commit; run `muninn sync`", repo)}
	}
	return filepath.Join(s.mirrorsDir, owner, name+".git"), rs.IndexedCommit, nil
}

// validRepoPart reports whether an owner or name component of a repo
// parameter is safe to join into a mirror path: non-empty, not a dot
// segment, and free of path separators.
func validRepoPart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, `/\`)
}

// parseLimit parses the limit query parameter, applying the default when
// absent or non-positive and clamping to the hard maximum.
func parseLimit(raw string) (int, error) {
	if raw == "" {
		return defaultSearchLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid limit %q: must be an integer", raw)
	}
	if limit <= 0 {
		return defaultSearchLimit, nil
	}
	if limit > maxSearchLimit {
		return maxSearchLimit, nil
	}
	return limit, nil
}

// trimResult maps a search result to the response shape, enforcing the
// line-match cap (a filename-only match counts as one result) and marking
// truncation when the cap cuts anything off.
func trimResult(res *search.Result, limit int) searchResponse {
	out := searchResponse{
		Files:     []fileMatchesJSON{},
		Truncated: res.Truncated,
		Stats: statsJSON{
			FilesConsidered: res.Stats.FilesConsidered,
			MatchCount:      res.Stats.MatchCount,
			DurationMs:      res.Stats.Duration.Milliseconds(),
		},
	}
	shown := 0
outer:
	for _, f := range res.Files {
		if shown >= limit {
			out.Truncated = true
			break
		}
		file := fileMatchesJSON{Repo: f.Repo, Path: f.Path, Lines: []lineMatchJSON{}}
		if len(f.Lines) == 0 {
			// Filename-only match: the file entry itself is the result.
			shown++
			out.Files = append(out.Files, file)
			continue
		}
		for _, l := range f.Lines {
			if shown >= limit {
				out.Truncated = true
				if len(file.Lines) > 0 {
					out.Files = append(out.Files, file)
				}
				break outer
			}
			file.Lines = append(file.Lines, lineMatchJSON{
				LineNumber:  l.LineNumber,
				Line:        l.Line,
				IsSymbolDef: l.IsSymbolDef,
			})
			shown++
		}
		out.Files = append(out.Files, file)
	}
	return out
}

// isBinary reports whether content looks binary: a NUL byte within the
// first 8 KiB (the same heuristic git uses).
func isBinary(content string) bool {
	head := content
	if len(head) > binarySniffBytes {
		head = head[:binarySniffBytes]
	}
	return bytes.IndexByte([]byte(head), 0) >= 0
}

// shortSHA abbreviates a commit SHA for display.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// formatAge renders a duration as a compact human-readable age.
func formatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	default:
		return d.Round(time.Hour).String()
	}
}

// writeJSON writes v as a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// Encoding errors past the header are unrecoverable mid-response;
	// the client sees a truncated body.
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON {"error": msg} response with the given status
// code.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// openRequest is the JSON body of POST /api/open. The client sends a
// repo/path pair; the server resolves the checkout itself and never accepts
// a filesystem path from the request.
type openRequest struct {
	Repo string `json:"repo"`
	Path string `json:"path"`
	Line int    `json:"line"`
}

// maxOpenBody caps the request body: the payload is three short fields.
const maxOpenBody = 8 << 10 // 8 KiB

// handleOpen serves POST /api/open, launching the editor with the repo's
// checkout loaded as the workspace folder. This is the only endpoint that
// starts a local process, so it carries its own CSRF guard: hostCheck does
// not cover it, because a form POST from a malicious page to 127.0.0.1
// carries an allowed Host header.
func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	if err := checkCSRF(r); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	var req openRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxOpenBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Rejected rather than coerced to 1: a missing line means the client
	// built a bad request, and silently opening the top of the file would
	// hide that.
	if req.Line < 1 {
		writeError(w, http.StatusBadRequest, "line must be a positive integer")
		return
	}
	checkout, ok := s.checkouts[strings.ToLower(req.Repo)]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no local checkout for %q", req.Repo))
		return
	}

	root, target, err := resolveInCheckout(checkout, req.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("%q is not in the local checkout (it may be on a different commit than the index)", req.Path))
		return
	case err != nil:
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	// VS Code and Cursor split the --goto value on ":" and read the first
	// numeric segment as a line number, so a path containing a colon can
	// resolve to the wrong file. Decline and let the client fall back.
	if strings.Contains(target, ":") {
		writeError(w, http.StatusNotImplemented, "path contains a colon and cannot be passed to --goto")
		return
	}

	// root, not the configured checkout path: the resolved form, so the
	// workspace folder and the file agree.
	switch err := s.launch(s.editorScheme, root, target, req.Line); {
	case errors.Is(err, ErrEditorCLINotFound):
		writeError(w, http.StatusNotImplemented, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkCSRF guards the one endpoint that launches a local process.
//
// Sec-Fetch-Site is browser-set and cannot be forged by page script. An
// absent header is rejected rather than trusted: treating it as safe would
// let any client that omits it bypass the guard entirely. Requiring JSON
// blocks simple form POSTs and forces a CORS preflight for a cross-origin
// fetch.
func checkCSRF(r *http.Request) error {
	if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
		return errors.New("forbidden: cross-origin or unrecognized request origin; reload the page and try again")
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return errors.New("forbidden: expected a JSON request")
	}
	return nil
}
