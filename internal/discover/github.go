// Package discover resolves the set of GitHub repositories to sync
// from the muninn configuration.
package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/broderick-westrope/muninn/internal/config"
)

// DefaultBaseURL is the GitHub REST API endpoint used when no override is set.
const DefaultBaseURL = "https://api.github.com"

// Repo is a resolved repository used by the rest of the sync pipeline.
type Repo struct {
	FullName      string // "owner/name"
	CloneURL      string // https
	DefaultBranch string
	Archived      bool
}

// Client talks to the GitHub REST API. The zero value uses DefaultBaseURL
// and http.DefaultClient; both fields are injectable for tests.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// Discover resolves all configured repositories using the default client.
func Discover(ctx context.Context, cfg *config.Config, token string) ([]Repo, error) {
	return (&Client{}).Discover(ctx, cfg, token)
}

// Discover lists org repositories and ad-hoc repos for every connection,
// applies each connection's exclusions, and returns the deduplicated set
// sorted by FullName.
func (c *Client) Discover(ctx context.Context, cfg *config.Config, token string) ([]Repo, error) {
	names := make([]string, 0, len(cfg.Connections))
	for name := range cfg.Connections {
		names = append(names, name)
	}
	sort.Strings(names)

	seen := make(map[string]bool)
	var repos []Repo
	for _, name := range names {
		conn := cfg.Connections[name]
		var connRepos []Repo
		for _, org := range conn.Orgs {
			rs, err := c.listOrgRepos(ctx, org, token)
			if err != nil {
				return nil, fmt.Errorf("connection %q: %w", name, err)
			}
			connRepos = append(connRepos, rs...)
		}
		for _, full := range conn.Repos {
			r, err := c.getRepo(ctx, full, token)
			if err != nil {
				return nil, fmt.Errorf("connection %q: %w", name, err)
			}
			connRepos = append(connRepos, r)
		}
		for _, r := range filterRepos(connRepos, conn.Exclude) {
			if seen[r.FullName] {
				continue
			}
			seen[r.FullName] = true
			repos = append(repos, r)
		}
	}

	sort.Slice(repos, func(i, j int) bool { return repos[i].FullName < repos[j].FullName })
	return repos, nil
}

func filterRepos(repos []Repo, exclude *config.Exclude) []Repo {
	if exclude == nil {
		return repos
	}
	excluded := make(map[string]bool, len(exclude.Repos))
	for _, name := range exclude.Repos {
		excluded[name] = true
	}
	filtered := repos[:0]
	for _, r := range repos {
		if exclude.Archived && r.Archived {
			continue
		}
		if excluded[r.FullName] {
			continue
		}
		filtered = append(filtered, r)
	}
	return filtered
}

type apiRepo struct {
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
}

func (r apiRepo) toRepo() Repo {
	return Repo{
		FullName:      r.FullName,
		CloneURL:      r.CloneURL,
		DefaultBranch: r.DefaultBranch,
		Archived:      r.Archived,
	}
}

func (c *Client) listOrgRepos(ctx context.Context, org, token string) ([]Repo, error) {
	url := c.baseURL() + "/orgs/" + org + "/repos?per_page=100"
	var repos []Repo
	for url != "" {
		body, header, err := c.get(ctx, url, token)
		if err != nil {
			return nil, fmt.Errorf("listing repos for org %s: %w", org, err)
		}
		var page []apiRepo
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decoding repo list for org %s: %w", org, err)
		}
		for _, r := range page {
			repos = append(repos, r.toRepo())
		}
		url = nextLink(header.Get("Link"))
	}
	return repos, nil
}

func (c *Client) getRepo(ctx context.Context, fullName, token string) (Repo, error) {
	if !strings.Contains(fullName, "/") {
		return Repo{}, fmt.Errorf("invalid repo %q: expected \"owner/name\"", fullName)
	}
	body, _, err := c.get(ctx, c.baseURL()+"/repos/"+fullName, token)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return Repo{}, fmt.Errorf("repo %s not found: check the name in the config's repos list and that the token has access: %w", fullName, err)
		}
		return Repo{}, fmt.Errorf("fetching repo %s: %w", fullName, err)
	}
	var r apiRepo
	if err := json.Unmarshal(body, &r); err != nil {
		return Repo{}, fmt.Errorf("decoding repo %s: %w", fullName, err)
	}
	return r.toRepo(), nil
}

type apiError struct {
	StatusCode int
	URL        string
	Body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d: %s", e.URL, e.StatusCode, e.Body)
}

// get performs an authenticated GET, retrying once on 403/429 while
// respecting the Retry-After header.
func (c *Client) get(ctx context.Context, url, token string) ([]byte, http.Header, error) {
	for attempt := 0; ; attempt++ {
		body, header, err := c.getOnce(ctx, url, token)
		var apiErr *apiError
		if err == nil || attempt > 0 || !errors.As(err, &apiErr) {
			return body, header, err
		}
		if apiErr.StatusCode != http.StatusForbidden && apiErr.StatusCode != http.StatusTooManyRequests {
			return nil, nil, err
		}
		delay := time.Second
		if secs, parseErr := strconv.Atoi(header.Get("Retry-After")); parseErr == nil && secs >= 0 {
			delay = time.Duration(secs) * time.Second
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (c *Client) getOnce(ctx context.Context, url, token string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("building request for %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("reading response from %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Header, &apiError{
			StatusCode: resp.StatusCode,
			URL:        url,
			Body:       strings.TrimSpace(string(body)),
		}
	}
	return body, resp.Header, nil
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

// nextLink extracts the rel="next" URL from a GitHub Link header,
// returning "" when there is no next page.
func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		segments := strings.Split(part, ";")
		if len(segments) < 2 {
			continue
		}
		url := strings.Trim(strings.TrimSpace(segments[0]), "<>")
		for _, seg := range segments[1:] {
			if strings.TrimSpace(seg) == `rel="next"` {
				return url
			}
		}
	}
	return ""
}
