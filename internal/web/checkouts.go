package web

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// ScanCheckouts walks each root up to 2 levels deep looking for git
// repositories (a .git entry, dir or file) and maps them by their
// origin URL's "owner/name" (lowercased) to the checkout path.
//
// The origin URL is read by parsing .git/config directly rather than
// shelling out to `git config` per repo: the file format is trivially
// line-oriented, and avoiding a process spawn per candidate keeps a scan
// of a large ~/dev tree fast and free of a git-binary dependency. Errors
// are skipped silently; the result is a best-effort map. Hidden
// directories are skipped, a matched repo is not descended into, and the
// first checkout found for a given repo wins.
func ScanCheckouts(roots []string) map[string]string {
	checkouts := make(map[string]string)
	for _, root := range roots {
		scanDir(root, 2, checkouts)
	}
	return checkouts
}

// scanDir records dir if it is a git checkout of a github repo, otherwise
// recurses into its non-hidden subdirectories while depth remains.
func scanDir(dir string, depth int, checkouts map[string]string) {
	if key, ok := originRepo(dir); ok {
		if _, exists := checkouts[key]; !exists {
			checkouts[key] = dir
		}
		// A repo's subdirectories belong to it; don't descend.
		return
	}
	if depth == 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		scanDir(filepath.Join(dir, e.Name()), depth-1, checkouts)
	}
}

// originRepo returns the lowercased "owner/name" of dir's github origin
// remote, or ok=false when dir is not a git checkout or its origin is not
// a github URL.
func originRepo(dir string) (string, bool) {
	configPath, ok := gitConfigPath(dir)
	if !ok {
		return "", false
	}
	url, ok := originURL(configPath)
	if !ok {
		return "", false
	}
	return parseGitHubRepo(url)
}

// gitConfigPath locates dir's git config file: .git/config for a normal
// checkout, or the config under the gitdir named by a .git file (worktree
// or submodule).
func gitConfigPath(dir string) (string, bool) {
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return filepath.Join(gitPath, "config"), true
	}
	// A .git file contains "gitdir: <path>", relative to dir when not
	// absolute.
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return "", false
	}
	gitdir, found := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !found {
		return "", false
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	return filepath.Join(gitdir, "config"), true
}

// originURL parses a git config file for the url of [remote "origin"].
func originURL(configPath string) (string, bool) {
	f, err := os.Open(configPath)
	if err != nil {
		return "", false
	}
	defer f.Close()

	inOrigin := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if !inOrigin {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if found && strings.TrimSpace(key) == "url" {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

// parseGitHubRepo normalizes a github remote URL to lowercased
// "owner/name". It handles scp-like SSH ("git@github.com:owner/name.git",
// including host aliases like "git@github.com-work:..."), HTTPS
// ("https://github.com/owner/name[.git]"), and explicit SSH
// ("ssh://git@github.com/owner/name.git"). Non-github remotes return
// ok=false.
func parseGitHubRepo(url string) (string, bool) {
	var host, path string
	if scheme, rest, found := strings.Cut(url, "://"); found {
		if scheme != "https" && scheme != "http" && scheme != "ssh" {
			return "", false
		}
		if _, hostpath, hasUser := strings.Cut(rest, "@"); hasUser {
			rest = hostpath
		}
		host, path, _ = strings.Cut(rest, "/")
		// Strip an SSH port if present (ssh://git@github.com:22/...).
		host, _, _ = strings.Cut(host, ":")
	} else if _, rest, hasUser := strings.Cut(url, "@"); hasUser {
		// scp-like: git@host:owner/name.git
		var found bool
		host, path, found = strings.Cut(rest, ":")
		if !found {
			return "", false
		}
	} else {
		return "", false
	}

	// SSH host aliases keep the real host as a prefix (github.com-work).
	if host != "github.com" && !strings.HasPrefix(host, "github.com-") {
		return "", false
	}

	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	owner, name, found := strings.Cut(path, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return strings.ToLower(owner + "/" + name), true
}
