package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/broderick-westrope/muninn/internal/config"
)

func githubConfig(conn config.Connection) *config.Config {
	return &config.Config{Connections: map[string]config.Connection{"gh": conn}}
}

func writeRepos(t *testing.T, w http.ResponseWriter, repos []apiRepo) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(repos); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}

func fullNames(repos []Repo) []string {
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = r.FullName
	}
	return names
}

func TestDiscoverPagination(t *testing.T) {
	pages := [][]apiRepo{
		{{FullName: "testorg/bravo", CloneURL: "https://example.com/testorg/bravo.git", DefaultBranch: "main"}},
		{{FullName: "testorg/alpha", CloneURL: "https://example.com/testorg/alpha.git", DefaultBranch: "master"}},
		{{FullName: "testorg/charlie", CloneURL: "https://example.com/testorg/charlie.git", DefaultBranch: "main"}},
	}
	var serverURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/testorg/repos", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q, want %q", got, "application/vnd.github+json")
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want %q", got, "100")
		}
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			fmt.Sscanf(p, "%d", &page)
		}
		if page < len(pages) {
			w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/testorg/repos?per_page=100&page=%d>; rel="next", <%s/orgs/testorg/repos?per_page=100&page=%d>; rel="last"`, serverURL, page+1, serverURL, len(pages)))
		}
		writeRepos(t, w, pages[page-1])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	serverURL = srv.URL

	c := &Client{BaseURL: srv.URL}
	repos, err := c.Discover(context.Background(), githubConfig(config.Connection{Type: "github", Orgs: []string{"testorg"}}), "test-token")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{"testorg/alpha", "testorg/bravo", "testorg/charlie"}
	if got := fullNames(repos); !equal(got, want) {
		t.Errorf("repos = %v, want %v", got, want)
	}
}

func TestDiscoverExcludes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/testorg/repos", func(w http.ResponseWriter, r *http.Request) {
		writeRepos(t, w, []apiRepo{
			{FullName: "testorg/keep", CloneURL: "https://example.com/testorg/keep.git", DefaultBranch: "main"},
			{FullName: "testorg/old", CloneURL: "https://example.com/testorg/old.git", DefaultBranch: "main", Archived: true},
			{FullName: "testorg/skipme", CloneURL: "https://example.com/testorg/skipme.git", DefaultBranch: "main"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	cfg := githubConfig(config.Connection{
		Type:    "github",
		Orgs:    []string{"testorg"},
		Exclude: &config.Exclude{Archived: true, Repos: []string{"testorg/skipme"}},
	})
	repos, err := c.Discover(context.Background(), cfg, "test-token")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{"testorg/keep"}
	if got := fullNames(repos); !equal(got, want) {
		t.Errorf("repos = %v, want %v", got, want)
	}
}

func TestDiscoverAdHocRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/testorg/repos", func(w http.ResponseWriter, r *http.Request) {
		writeRepos(t, w, []apiRepo{
			{FullName: "testorg/main", CloneURL: "https://example.com/testorg/main.git", DefaultBranch: "main"},
		})
	})
	mux.HandleFunc("/repos/solo/extra", func(w http.ResponseWriter, r *http.Request) {
		repo := apiRepo{FullName: "solo/extra", CloneURL: "https://example.com/solo/extra.git", DefaultBranch: "trunk"}
		if err := json.NewEncoder(w).Encode(repo); err != nil {
			t.Errorf("encoding response: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	cfg := githubConfig(config.Connection{Type: "github", Orgs: []string{"testorg"}, Repos: []string{"solo/extra"}})
	repos, err := c.Discover(context.Background(), cfg, "test-token")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{"solo/extra", "testorg/main"}
	if got := fullNames(repos); !equal(got, want) {
		t.Errorf("repos = %v, want %v", got, want)
	}
	if repos[0].DefaultBranch != "trunk" {
		t.Errorf("DefaultBranch = %q, want %q", repos[0].DefaultBranch, "trunk")
	}
}

func TestDiscoverAdHocNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/solo/missing", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message": "Not Found"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	cfg := githubConfig(config.Connection{Type: "github", Repos: []string{"solo/missing"}})
	_, err := c.Discover(context.Background(), cfg, "test-token")
	if err == nil {
		t.Fatal("Discover: expected error, got nil")
	}
	for _, want := range []string{"solo/missing", "not found", "Not Found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestDiscoverRetriesRateLimit(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/testorg/repos", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message": "rate limited"}`)
			return
		}
		writeRepos(t, w, []apiRepo{
			{FullName: "testorg/main", CloneURL: "https://example.com/testorg/main.git", DefaultBranch: "main"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	repos, err := c.Discover(context.Background(), githubConfig(config.Connection{Type: "github", Orgs: []string{"testorg"}}), "test-token")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if got := fullNames(repos); !equal(got, []string{"testorg/main"}) {
		t.Errorf("repos = %v, want [testorg/main]", got)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
