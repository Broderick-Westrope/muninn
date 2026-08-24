// Package mirror manages bare git mirrors of GitHub repositories,
// laid out as <baseDir>/<owner>/<name>.git.
package mirror

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/broderick-westrope/muninn/internal/discover"
)

// Manager performs git operations on the mirrors under BaseDir.
// Callers pass the base directory (typically xdg.MirrorsDir()).
type Manager struct {
	BaseDir string
}

// Dir returns the mirror directory for a repo's "owner/name".
func (m *Manager) Dir(fullName string) string {
	owner, name, _ := strings.Cut(fullName, "/")
	return filepath.Join(m.BaseDir, owner, name+".git")
}

// Ensure clones the repo as a bare mirror if it is missing, otherwise
// fetches with prune. It reports whether a new mirror was created.
//
// It deliberately avoids `git clone --mirror`: --mirror implies the
// +refs/*:refs/* refspec, so `fetch --prune` would delete
// refs/muninn/indexed on every sync (it matches refs/* and does not
// exist on the remote) and would pull GitHub's refs/pull/* bloat.
// Instead the fetch refspec is narrowed to +refs/heads/*:refs/heads/*,
// so prune only touches refs/heads/* and refs/muninn/* is never a
// fetch destination.
func (m *Manager) Ensure(ctx context.Context, repo discover.Repo, token string) (created bool, err error) {
	dir := m.Dir(repo.FullName)
	env := authEnv(token)

	if _, statErr := os.Stat(dir); statErr == nil {
		// Re-assert the config before fetching so mirrors created by older
		// versions (which set config after clone and could die in between)
		// self-heal instead of staying silently stale.
		if err := assertConfig(ctx, dir, repo.FullName); err != nil {
			return false, err
		}
		if _, err := runGit(ctx, env, "-C", dir, "fetch", "--prune", "origin"); err != nil {
			return false, fmt.Errorf("fetching %s: %w", repo.FullName, err)
		}
		return false, nil
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return false, fmt.Errorf("checking mirror %s: %w", dir, statErr)
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return false, fmt.Errorf("creating mirror directory for %s: %w", repo.FullName, err)
	}
	// The mirror is created atomically: clone into a temp sibling, set the
	// fetch refspec there, then rename into place. A process death at any
	// point leaves either no mirror (re-cloned next run) or a complete one
	// — never a mirror without a fetch refspec, which would be silently
	// stale forever. Setting the refspec via `clone --config` is not
	// possible: it duplicates the bare clone's internal refs/heads mapping
	// ("multiple updates for ref ... not allowed"). gc.auto=0 is a backstop
	// protecting indexed commits from auto-gc; refs/muninn/indexed is the
	// primary mechanism. The fixed temp name does not collide because sync
	// never runs Ensure for the same repo concurrently within a run.
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return false, fmt.Errorf("removing stale temp clone for %s: %w", repo.FullName, err)
	}
	if _, err := runGit(ctx, env, "clone", "--bare", "--config", "gc.auto=0", repo.CloneURL, tmp); err != nil {
		return false, fmt.Errorf("cloning %s: %w", repo.FullName, err)
	}
	if _, err := runGit(ctx, nil, "-C", tmp, "config", "remote.origin.fetch", "+refs/heads/*:refs/heads/*"); err != nil {
		return false, fmt.Errorf("configuring fetch refspec for %s: %w", repo.FullName, err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		return false, fmt.Errorf("moving mirror of %s into place: %w", repo.FullName, err)
	}
	return true, nil
}

// assertConfig (re)applies the fetch refspec and gc backstop on an existing
// mirror, healing mirrors left partial by a crash or created before the
// config was passed atomically on clone.
func assertConfig(ctx context.Context, dir, fullName string) error {
	if _, err := runGit(ctx, nil, "-C", dir, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/*:refs/heads/*"); err != nil {
		return fmt.Errorf("configuring fetch refspec for %s: %w", fullName, err)
	}
	if _, err := runGit(ctx, nil, "-C", dir, "config", "gc.auto", "0"); err != nil {
		return fmt.Errorf("disabling auto-gc for %s: %w", fullName, err)
	}
	return nil
}

// HeadCommit returns the commit SHA at the tip of the default branch.
func (m *Manager) HeadCommit(ctx context.Context, dir, defaultBranch string) (string, error) {
	sha, err := runGit(ctx, nil, "-C", dir, "rev-parse", "refs/heads/"+defaultBranch)
	if err != nil {
		return "", fmt.Errorf("resolving head of %s in %s: %w", defaultBranch, dir, err)
	}
	return sha, nil
}

// MarkIndexed records the indexed commit under refs/muninn/indexed,
// keeping it reachable across upstream force-pushes and gc.
func (m *Manager) MarkIndexed(ctx context.Context, dir, sha string) error {
	if _, err := runGit(ctx, nil, "-C", dir, "update-ref", "refs/muninn/indexed", sha); err != nil {
		return fmt.Errorf("marking indexed commit in %s: %w", dir, err)
	}
	return nil
}

// List returns the "owner/name" of every mirror on disk, sorted.
func (m *Manager) List() ([]string, error) {
	owners, err := os.ReadDir(m.BaseDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading mirrors directory %s: %w", m.BaseDir, err)
	}
	var repos []string
	for _, owner := range owners {
		if !owner.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(m.BaseDir, owner.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading owner directory %s: %w", owner.Name(), err)
		}
		for _, entry := range entries {
			name, ok := strings.CutSuffix(entry.Name(), ".git")
			if !ok || !entry.IsDir() {
				continue
			}
			repos = append(repos, owner.Name()+"/"+name)
		}
	}
	sort.Strings(repos)
	return repos, nil
}

// Remove deletes the mirror for fullName and prunes the owner directory
// if it is now empty.
func (m *Manager) Remove(fullName string) error {
	dir := m.Dir(fullName)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing mirror %s: %w", fullName, err)
	}
	// Best-effort prune; fails harmlessly if the owner dir is not empty.
	os.Remove(filepath.Dir(dir))
	return nil
}

// CleanTmp removes orphaned temp clone directories left behind by
// interrupted or killed clones. Safe to call only while no clone is in
// flight (sync calls it before starting the worker pool).
func (m *Manager) CleanTmp() error {
	owners, err := os.ReadDir(m.BaseDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading mirrors directory %s: %w", m.BaseDir, err)
	}
	for _, owner := range owners {
		if !owner.IsDir() {
			continue
		}
		ownerDir := filepath.Join(m.BaseDir, owner.Name())
		entries, err := os.ReadDir(ownerDir)
		if err != nil {
			return fmt.Errorf("reading owner directory %s: %w", owner.Name(), err)
		}
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasSuffix(entry.Name(), ".git.tmp") {
				continue
			}
			if err := os.RemoveAll(filepath.Join(ownerDir, entry.Name())); err != nil {
				return fmt.Errorf("removing orphaned temp clone %s: %w", entry.Name(), err)
			}
		}
	}
	return nil
}

// authEnv returns the extra environment for network git operations. It
// injects the token as an HTTP header via git config env vars, so it
// never appears in argv or on disk. GitHub's git endpoint rejects Bearer
// for OAuth/gh tokens, so Basic auth with the x-access-token username is
// used (works for all token types). It also sets a low-speed abort so a
// stalled transfer (dead connection, throttling) fails after a minute
// instead of wedging a sync worker indefinitely — git/curl have no
// default stall timeout. Auth is omitted for empty tokens; the low-speed
// abort is always applied.
func authEnv(token string) []string {
	env := []string{
		"GIT_CONFIG_KEY_0=http.lowSpeedLimit",
		"GIT_CONFIG_VALUE_0=1000",
		"GIT_CONFIG_KEY_1=http.lowSpeedTime",
		"GIT_CONFIG_VALUE_1=60",
	}
	if token == "" {
		return append(env, "GIT_CONFIG_COUNT=2")
	}
	credentials := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return append(env,
		"GIT_CONFIG_KEY_2=http.extraHeader",
		"GIT_CONFIG_VALUE_2=Authorization: Basic "+credentials,
		"GIT_CONFIG_COUNT=3",
	)
}

func runGit(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
