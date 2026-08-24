// Package sync orchestrates the reconciliation pipeline: discover the
// desired repos, GC removed ones, mirror and index the rest with bounded
// concurrency, and record per-repo results in the status file.
package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	stdsync "sync"
	"time"

	"github.com/broderick-westrope/muninn/internal/config"
	"github.com/broderick-westrope/muninn/internal/ctags"
	"github.com/broderick-westrope/muninn/internal/discover"
	"github.com/broderick-westrope/muninn/internal/status"
)

const (
	defaultFetchConcurrency = 8
	defaultIndexConcurrency = 2
)

// Discoverer resolves the desired set of repositories.
type Discoverer interface {
	Discover(ctx context.Context, cfg *config.Config, token string) ([]discover.Repo, error)
}

// Mirror manages bare git mirrors (implemented by *mirror.Manager).
type Mirror interface {
	Dir(fullName string) string
	Ensure(ctx context.Context, repo discover.Repo, token string) (created bool, err error)
	HeadCommit(ctx context.Context, dir, defaultBranch string) (string, error)
	MarkIndexed(ctx context.Context, dir, sha string) error
	List() ([]string, error)
	Remove(fullName string) error
	CleanTmp() error
}

// Indexer manages Zoekt shards (implemented by *index.Indexer).
type Indexer interface {
	IndexRepo(ctx context.Context, mirrorDir, fullName, branch, commit string) error
	CleanTmp() error
	RemoveShards(fullName string) error
	ListIndexed() (map[string]string, error)
}

// Deps carries the pipeline's collaborators so tests can stub them.
type Deps struct {
	Discoverer Discoverer
	Mirror     Mirror
	// NewIndexer constructs the indexer once the ctags binary is resolved.
	NewIndexer func(ctagsPath string) Indexer
	// ResolveCtags resolves and validates the ctags binary; nil means
	// ctags.Resolve.
	ResolveCtags func(configured string) (string, error)
	Token        string
	StatusPath   string
	// FetchConcurrency bounds concurrent clones/fetches; 0 means 8.
	FetchConcurrency int
	// IndexConcurrency bounds concurrent Zoekt builds; 0 means 2.
	IndexConcurrency int
}

// Run executes one sync: seed status from the previous run, validate ctags,
// discover repos, GC removed mirrors and shards, then fetch and index every
// repo with per-repo failure isolation. The status file is rewritten after
// each repo completes so readers see at most a few seconds of skew. It
// returns a non-nil error only on total failure; per-repo failures are
// recorded in the returned status with Success=false.
func Run(ctx context.Context, cfg *config.Config, deps Deps) (*status.SyncStatus, error) {
	if deps.Discoverer == nil || deps.Mirror == nil || deps.NewIndexer == nil || deps.StatusPath == "" {
		return nil, errors.New("sync: Deps requires Discoverer, Mirror, NewIndexer, and StatusPath")
	}
	fetchN := deps.FetchConcurrency
	if fetchN <= 0 {
		fetchN = defaultFetchConcurrency
	}
	indexN := deps.IndexConcurrency
	if indexN <= 0 {
		indexN = defaultIndexConcurrency
	}
	resolveCtags := deps.ResolveCtags
	if resolveCtags == nil {
		resolveCtags = ctags.Resolve
	}

	// Seed from the previous run so IndexedCommit carries forward when a
	// repo (or the whole run) fails before indexing a new tip.
	prev, err := status.Read(deps.StatusPath)
	if err != nil && !errors.Is(err, status.ErrNotExist) {
		return nil, fmt.Errorf("reading previous status: %w", err)
	}
	prevRepos := map[string]status.RepoStatus{}
	if prev != nil && prev.Repos != nil {
		prevRepos = prev.Repos
	}

	st := &status.SyncStatus{StartedAt: time.Now(), Repos: make(map[string]status.RepoStatus)}

	// ctags is a hard prerequisite: fail the whole run loudly rather than
	// silently building shards without symbols.
	ctagsPath, err := resolveCtags(cfg.Ctags.Path)
	if err != nil {
		return nil, fmt.Errorf("validating ctags: %w", err)
	}
	ix := deps.NewIndexer(ctagsPath)

	repos, err := deps.Discoverer.Discover(ctx, cfg, deps.Token)
	if err != nil {
		// Without the full desired list reconciliation cannot GC safely.
		return abortRun(deps.StatusPath, st, prevRepos, fmt.Errorf("discovering repos: %w", err))
	}

	desired := make(map[string]bool, len(repos))
	for _, r := range repos {
		desired[r.FullName] = true
	}
	// Seed entries for the desired repos so a mid-run reader sees the
	// last-known state; removed repos never enter and are thus dropped.
	for _, r := range repos {
		if rs, ok := prevRepos[r.FullName]; ok {
			st.Repos[r.FullName] = rs
		}
	}

	if err := reconcile(deps.Mirror, ix, desired, prevRepos, st); err != nil {
		return abortRun(deps.StatusPath, st, prevRepos, err)
	}
	if err := ix.CleanTmp(); err != nil {
		return abortRun(deps.StatusPath, st, prevRepos, fmt.Errorf("cleaning stale tmp shards: %w", err))
	}
	if err := deps.Mirror.CleanTmp(); err != nil {
		return abortRun(deps.StatusPath, st, prevRepos, fmt.Errorf("cleaning orphaned temp clones: %w", err))
	}

	fetchSem := make(chan struct{}, fetchN)
	indexSem := make(chan struct{}, indexN)
	var (
		wg          stdsync.WaitGroup
		mu          stdsync.Mutex // guards st.Repos, incWriteErr, and status file writes
		incWriteErr error
	)
	for _, repo := range repos {
		wg.Add(1)
		go func(repo discover.Repo) {
			defer wg.Done()
			rs := syncRepo(ctx, deps, ix, repo, prevRepos[repo.FullName], fetchSem, indexSem)
			mu.Lock()
			st.Repos[repo.FullName] = rs
			// Incremental write (best effort): keeps a live MCP session's
			// shard/status skew window to seconds, not the whole run. The
			// first failure is recorded and surfaced after the run.
			if werr := status.Write(deps.StatusPath, st); werr != nil && incWriteErr == nil {
				incWriteErr = werr
			}
			mu.Unlock()
		}(repo)
	}
	wg.Wait()
	if incWriteErr != nil {
		fmt.Fprintf(os.Stderr, "muninn: incremental status write failed: %v\n", incWriteErr)
	}

	st.FinishedAt = time.Now()
	st.Success = true
	for _, rs := range st.Repos {
		if rs.Error != "" {
			st.Success = false
			break
		}
	}
	if err := status.Write(deps.StatusPath, st); err != nil {
		return st, fmt.Errorf("writing final status: %w", err)
	}
	return st, nil
}

// abortRun writes a failed status before returning err, retaining the
// previous per-repo entries for any repo not already recorded, so an
// aborted run is never silent (the spec requires every run to end with a
// status write).
func abortRun(path string, st *status.SyncStatus, prevRepos map[string]status.RepoStatus, err error) (*status.SyncStatus, error) {
	for name, rs := range prevRepos {
		if _, ok := st.Repos[name]; !ok {
			st.Repos[name] = rs
		}
	}
	st.FinishedAt = time.Now()
	if werr := status.Write(path, st); werr != nil {
		return st, fmt.Errorf("%w (also failed writing status: %v)", err, werr)
	}
	return st, err
}

// reconcile removes mirrors and shards for repos no longer desired. Actual
// state is the union of mirrors on disk and indexed shards, so orphans of
// either kind are collected. A per-repo removal failure does not abort the
// run: it is recorded in st.Repos (retaining the repo's previous entry with
// the error) and GC continues; only listing failures return an error.
func reconcile(m Mirror, ix Indexer, desired map[string]bool, prevRepos map[string]status.RepoStatus, st *status.SyncStatus) error {
	mirrored, err := m.List()
	if err != nil {
		return fmt.Errorf("listing mirrors: %w", err)
	}
	indexed, err := ix.ListIndexed()
	if err != nil {
		return fmt.Errorf("listing indexed repos: %w", err)
	}
	actual := make(map[string]bool, len(mirrored)+len(indexed))
	for _, name := range mirrored {
		actual[name] = true
	}
	for name := range indexed {
		actual[name] = true
	}
	var removed []string
	for name := range actual {
		if !desired[name] {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	for _, name := range removed {
		var errs []string
		if err := m.Remove(name); err != nil {
			errs = append(errs, fmt.Sprintf("removing mirror: %v", err))
		}
		if err := ix.RemoveShards(name); err != nil {
			errs = append(errs, fmt.Sprintf("removing shards: %v", err))
		}
		if len(errs) > 0 {
			rs := prevRepos[name]
			rs.Error = strings.Join(errs, "; ")
			st.Repos[name] = rs
		}
	}
	return nil
}

// syncRepo fetches and indexes one repo, returning its status. Any error is
// captured into RepoStatus.Error and the previously indexed commit retained,
// so a failure never loses the read_file pinning guarantee.
func syncRepo(ctx context.Context, deps Deps, ix Indexer, repo discover.Repo, prev status.RepoStatus, fetchSem, indexSem chan struct{}) status.RepoStatus {
	rs := status.RepoStatus{IndexedCommit: prev.IndexedCommit}

	fetchSem <- struct{}{}
	_, err := deps.Mirror.Ensure(ctx, repo, deps.Token)
	<-fetchSem
	if err != nil {
		rs.Error = err.Error()
		return rs
	}
	rs.Fetched = true

	dir := deps.Mirror.Dir(repo.FullName)
	head, err := deps.Mirror.HeadCommit(ctx, dir, repo.DefaultBranch)
	if err != nil {
		rs.Error = err.Error()
		return rs
	}

	// Invariant: IndexRepo indexes the branch tip, which equals the head we
	// just resolved only because no concurrent fetch of the same repo can
	// occur within a run.
	indexSem <- struct{}{}
	err = ix.IndexRepo(ctx, dir, repo.FullName, repo.DefaultBranch, head)
	<-indexSem
	if err != nil {
		rs.Error = err.Error()
		return rs
	}
	if err := deps.Mirror.MarkIndexed(ctx, dir, head); err != nil {
		rs.Error = err.Error()
		return rs
	}
	rs.Indexed = true
	rs.IndexedCommit = head
	return rs
}
