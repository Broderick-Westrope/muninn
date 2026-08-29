// Package githistory answers history questions over bare git mirrors:
// commit search with pickaxe filters (SearchCommits), commit and range
// diffs with agent-safe truncation (GetDiff), and pinned-rev line
// attribution (Blame). All operations are read-only, run against a bare
// mirror, and shell out to git through the hermetic gitcmd runner under
// per-operation deadlines.
package githistory

import (
	"fmt"
	"strings"
	"time"

	"github.com/broderick-westrope/muninn/internal/gitcmd"
)

// Per-operation deadlines. SearchCommits supports labeled partial results
// on timeout; GetDiff and Blame do not — their timeouts surface as errors
// naming the narrowing options.
const (
	logTimeout   = 15 * time.Second
	diffTimeout  = 60 * time.Second
	blameTimeout = 60 * time.Second
)

// Commit is one row of SearchCommits output.
type Commit struct {
	// SHA is the full commit SHA.
	SHA string
	// AuthorDate is the author date in YYYY-MM-DD form (git %as).
	AuthorDate string
	// Author is the author name.
	Author string
	// Subject is the first line of the commit message.
	Subject string
	// IsMerge reports whether the commit has more than one parent.
	IsMerge bool
}

// runner returns the package's hermetic git runner for the given deadline.
// GIT_LITERAL_PATHSPECS=1 makes every path argument a literal path, never a
// glob — which is also what --follow requires. It rides in ExtraEnv because
// it is a plain environment toggle, not a GIT_CONFIG_-shaped value, so the
// runner's hermeticity filtering leaves it alone.
func runner(timeout time.Duration) gitcmd.Runner {
	return gitcmd.Runner{
		Timeout:  timeout,
		ExtraEnv: []string{"GIT_LITERAL_PATHSPECS=1"},
	}
}

// validatePath rejects path arguments that git could mistake for options
// or pathspec magic. GIT_LITERAL_PATHSPECS already disables magic, but a
// leading '-' would still parse as an option before --end-of-options takes
// effect for pathspecs, so both are rejected outright.
func validatePath(path string) error {
	if strings.HasPrefix(path, "-") || strings.HasPrefix(path, ":") {
		return fmt.Errorf("path %q must not start with '-' or ':'", path)
	}
	return nil
}

// shortSHA abbreviates a commit SHA for error and warning messages.
func shortSHA(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
