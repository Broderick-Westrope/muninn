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
	"github.com/broderick-westrope/muninn/internal/gitcmd"
	"github.com/broderick-westrope/muninn/internal/lock"
	"github.com/broderick-westrope/muninn/internal/status"
)

const (
	defaultFetchConcurrency = 8
	defaultIndexConcurrency = 2
)

// Network git operations against github.com fail sporadically at a low
// rate — observed as `SSL_connect: SSL_ERROR_SYSCALL`, a peer vanishing
// mid-handshake with no TLS alert — and succeed immediately on a second
// attempt. Without a retry a single dropped handshake fails that repo for
// the whole run, and one failed repo is enough to mark the run degraded.
//
// maxFetchRetryRepos bounds how many repos in one run may retry, because
// the interesting case is the one this does not help: if the token died or
// GitHub is down then every repo fails, and retrying all of them would
// double the load against an already failing endpoint while turning a
// clear total failure into a slow partial one. A handful of flakes stays
// under the budget; a fleet-wide outage blows through it and the run fails
// fast, as it should.
const maxFetchRetryRepos = 16

// fetchRetryDelay is the backoff before a retried fetch. It is a var so
// tests can zero it rather than sleeping through it.
var fetchRetryDelay = 2 * time.Second

// ErrLocked reports that another sync process holds the lock, so this run
// did nothing. It is a normal outcome, not a failure: callers should report
// it and exit successfully.
var ErrLocked = errors.New("another sync is already running")

// ErrNotDue reports that OnlyIfDue was set and the index is fresh enough,
// so this run did nothing. Also a normal outcome.
var ErrNotDue = errors.New("sync is not due yet")

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
	MaybeGC(ctx context.Context, dir string) (ran bool, err error)
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
	// LockPath is the advisory lock file that serializes runs. Required:
	// two concurrent runs corrupt each other's temp files and clobber the
	// status file.
	LockPath string
	// OnlyIfDue skips the run when the index is fresh, per status.Due and
	// the cadences in cfg.Sync. Scheduled runs set it; a manual `muninn
	// sync` does not.
	OnlyIfDue bool
	// Now supplies the current time for the due check; nil means
	// time.Now.
	Now func() time.Time
	// FetchConcurrency bounds concurrent clones/fetches; 0 means 8.
	FetchConcurrency int
	// IndexConcurrency bounds concurrent Zoekt builds; 0 means 2.
	IndexConcurrency int
}

// Run executes one sync: take the exclusive lock, seed status from the
// previous run, validate ctags, discover repos, GC removed mirrors and
// shards, then fetch and index every repo with per-repo failure isolation.
// The status file is rewritten after each repo completes so readers see at
// most a few seconds of skew.
//
// It returns a non-nil error only on total failure; per-repo failures are
// recorded in the returned status with Success=false. ErrLocked and
// ErrNotDue mean the run deliberately did nothing and are returned with the
// existing status (which may be nil) for reporting.
func Run(ctx context.Context, cfg *config.Config, deps Deps) (*status.SyncStatus, error) {
	if deps.Discoverer == nil || deps.Mirror == nil || deps.NewIndexer == nil || deps.StatusPath == "" || deps.LockPath == "" {
		return nil, errors.New("sync: Deps requires Discoverer, Mirror, NewIndexer, StatusPath, and LockPath")
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
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	// The lock is taken before the status read so the due check below sees
	// state no other run can be mutating: without it two schedulers firing
	// together would both read "due" and both proceed.
	lk, err := lock.TryAcquire(deps.LockPath)
	switch {
	case errors.Is(err, lock.ErrLocked):
		prev, _ := status.Read(deps.StatusPath)
		return prev, fmt.Errorf("%w: %w", ErrLocked, err)
	case errors.Is(err, lock.ErrUnsupported):
		// Losing the concurrency guarantee is not a reason to stop
		// indexing; the caller warns and the run proceeds unprotected.
		fmt.Fprintf(os.Stderr, "muninn: %v; concurrent syncs are not prevented\n", err)
	case err != nil:
		// Every run must end with a status write, or a scheduled job that
		// dies here is indistinguishable from one that never fired.
		st := &status.SyncStatus{StartedAt: now(), Repos: make(map[string]status.RepoStatus)}
		prev, _ := status.Read(deps.StatusPath)
		return abortFromPrev(deps.StatusPath, st, prev, now(), err)
	}
	defer func() {
		if err := lk.Release(); err != nil {
			fmt.Fprintf(os.Stderr, "muninn: releasing sync lock: %v\n", err)
		}
	}()

	// Seed from the previous run so IndexedCommit carries forward when a
	// repo (or the whole run) fails before indexing a new tip.
	prev, err := status.Read(deps.StatusPath)
	if err != nil && !errors.Is(err, status.ErrNotExist) {
		// A corrupt status file must still be recorded as a failed run:
		// abortRun overwrites it, which also repairs the corruption.
		st := &status.SyncStatus{StartedAt: now(), Repos: make(map[string]status.RepoStatus)}
		return abortRun(deps.StatusPath, st, nil, preReconcile, now(), fmt.Errorf("reading previous status: %w", err))
	}
	prevRepos := map[string]status.RepoStatus{}
	if prev != nil && prev.Repos != nil {
		prevRepos = prev.Repos
	}

	if deps.OnlyIfDue && !status.Due(prev, cfg.Sync.Interval(), cfg.Sync.RetryInterval(), now()) {
		return prev, ErrNotDue
	}

	st := &status.SyncStatus{StartedAt: now(), Repos: make(map[string]status.RepoStatus)}
	if prev != nil {
		// Carry the success clock and failure count forward: a run that
		// fails must not make the index look fresh, and a mid-run reader
		// must still see the real staleness while FinishedAt is zero.
		st.LastSuccessAt = prev.LastSuccessAt
		st.ConsecutiveFailures = prev.ConsecutiveFailures
		st.Error = prev.Error
		// A previous run that started but never finished died before it
		// could record anything. Holding the lock proves it is not still
		// running, so count it now: otherwise a sync killed every time
		// (OOM, sleep) leaves the failure count frozen and the cause
		// blank, which reads as healthy until the index goes stale.
		if status.Interrupted(prev) {
			st.ConsecutiveFailures++
			st.Error = "previous run was interrupted before it finished"
		}
	}

	// ctags is a hard prerequisite: fail the whole run loudly rather than
	// silently building shards without symbols. This goes through abortRun
	// like any other total failure — a ctags path baked into the config at
	// install time can break every scheduled run, and a run that fails
	// without recording anything is indistinguishable from one that never
	// fired.
	ctagsPath, err := resolveCtags(cfg.Ctags.Path)
	if err != nil {
		return abortRun(deps.StatusPath, st, prevRepos, preReconcile, now(), fmt.Errorf("validating ctags: %w", err))
	}
	ix := deps.NewIndexer(ctagsPath)

	repos, err := deps.Discoverer.Discover(ctx, cfg, deps.Token)
	if err != nil {
		// Without the full desired list reconciliation cannot GC safely.
		return abortRun(deps.StatusPath, st, prevRepos, preReconcile, now(), fmt.Errorf("discovering repos: %w", err))
	}
	// An empty result is treated as a failed discovery rather than an
	// instruction to delete everything. The config is validated to name at
	// least one org or repo, so "nothing desired" means discovery lied:
	// most plausibly a token that lost org visibility, which the GitHub
	// API reports as an empty list rather than an error. Obeying it would
	// GC every mirror and shard and then record a fully successful,
	// perfectly fresh sync over an empty index.
	if len(repos) == 0 {
		return abortRun(deps.StatusPath, st, prevRepos, preReconcile, now(),
			errors.New("discovery returned no repositories; refusing to GC the whole index (check the token's org visibility)"))
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
		// reconcile may have deleted some repos before failing, so the
		// backfill is filtered from here on.
		return abortRun(deps.StatusPath, st, prevRepos, desired, now(), err)
	}
	if err := ix.CleanTmp(); err != nil {
		return abortRun(deps.StatusPath, st, prevRepos, desired, now(), fmt.Errorf("cleaning stale tmp shards: %w", err))
	}
	if err := deps.Mirror.CleanTmp(); err != nil {
		return abortRun(deps.StatusPath, st, prevRepos, desired, now(), fmt.Errorf("cleaning orphaned temp clones: %w", err))
	}

	fetchSem := make(chan struct{}, fetchN)
	indexSem := make(chan struct{}, indexN)
	budget := &retryBudget{left: maxFetchRetryRepos}
	var (
		wg          stdsync.WaitGroup
		mu          stdsync.Mutex // guards st.Repos, incWriteErr, and status file writes
		incWriteErr error
	)
	for _, repo := range repos {
		wg.Add(1)
		go func(repo discover.Repo) {
			defer wg.Done()
			rs := syncRepo(ctx, deps, ix, repo, prevRepos[repo.FullName], now, fetchSem, indexSem, budget)
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

	st.FinishedAt = now()
	failed := 0
	for _, rs := range st.Repos {
		if rs.Error != "" {
			failed++
		}
	}
	// The run reached the end of the pipeline, so the mechanism works even
	// if individual repos did not. TotalFailure stays false, which keeps
	// the scheduler on the normal cadence: a repo that will never succeed
	// must not drive a full-fleet refetch every few minutes.
	st.TotalFailure = false
	if failed == 0 {
		// The fleet-wide clock advances only on a clean sweep; per-repo
		// clocks are set in syncRepo and are what staleness reads. A
		// recorded GCError does not count: the repo is fetched, indexed,
		// and pinned, so its data is genuinely fresh.
		st.Success = true
		st.LastSuccessAt = st.FinishedAt
		st.ConsecutiveFailures = 0
		st.Error = ""
	} else {
		st.Success = false
		st.Error = fmt.Sprintf("%d of %d repos failed", failed, len(st.Repos))
		st.ConsecutiveFailures++
	}
	if err := status.Write(deps.StatusPath, st); err != nil {
		return st, fmt.Errorf("writing final status: %w", err)
	}
	return st, nil
}

// abortFromPrev records a failure that happened before the previous status
// could be seeded into st, carrying the success clock and failure count
// forward from prev so the freshness clock survives.
func abortFromPrev(path string, st *status.SyncStatus, prev *status.SyncStatus, now time.Time, err error) (*status.SyncStatus, error) {
	var prevRepos map[string]status.RepoStatus
	if prev != nil {
		st.LastSuccessAt = prev.LastSuccessAt
		st.ConsecutiveFailures = prev.ConsecutiveFailures
		prevRepos = prev.Repos
	}
	return abortRun(path, st, prevRepos, preReconcile, now, err)
}

// preReconcile is passed as abortRun's desired set for failures that occur
// before reconcile runs, where nothing has been deleted yet and every
// previous entry should be retained.
var preReconcile map[string]bool

// abortRun writes a failed status before returning err, retaining the
// previous per-repo entries for any repo not already recorded, so an
// aborted run is never silent (the spec requires every run to end with a
// status write). LastSuccessAt is left as seeded: a total failure moves the
// attempt clock but never the freshness clock.
//
// desired filters what is retained. Once reconcile has run it has already
// deleted the mirrors and shards of undesired repos, so backfilling them
// would resurrect entries claiming a repo is indexed at a commit whose
// shard no longer exists. Pass preReconcile when nothing can have been
// deleted yet.
//
// TotalFailure is set: reaching here means the run failed as a whole
// rather than in individual repos, which is what puts the scheduler on the
// faster retry cadence.
func abortRun(path string, st *status.SyncStatus, prevRepos map[string]status.RepoStatus, desired map[string]bool, now time.Time, err error) (*status.SyncStatus, error) {
	for name, rs := range prevRepos {
		if desired != nil && !desired[name] {
			continue
		}
		if _, ok := st.Repos[name]; !ok {
			st.Repos[name] = rs
		}
	}
	st.FinishedAt = now
	st.Success = false
	st.TotalFailure = true
	st.Error = status.TruncateError(err.Error())
	st.ConsecutiveFailures++
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
			// The repo is no longer desired and its removal failed, so it
			// is retained only to report that error. Its shards may already
			// be gone (RemoveShards can succeed while Remove fails), so the
			// freshness clock is cleared: an unsearchable repo must not be
			// named as stale data in every tool response.
			rs := prevRepos[name]
			rs.LastSuccessAt = time.Time{}
			rs.Error = status.TruncateError(strings.Join(errs, "; "))
			st.Repos[name] = rs
		}
	}
	return nil
}

// retryBudget is a fleet-wide allowance for fetch retries within one run,
// shared by every repo goroutine.
type retryBudget struct {
	mu   stdsync.Mutex
	left int
}

// take consumes one retry, reporting whether the budget allowed it.
func (b *retryBudget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.left <= 0 {
		return false
	}
	b.left--
	return true
}

// ensureMirror fetches or clones one repo, retrying once on a transient
// failure. It reports how many retries were spent so a rising flake rate
// stays visible instead of being silently absorbed.
//
// The fetch semaphore is released around the backoff: holding a slot while
// sleeping would shrink the pool exactly when it is contended, and during
// a wide outage every worker would sleep while holding one, stalling the
// run without doing any work.
//
// A timeout is never retried. It has already consumed the full fetch
// deadline, so a second attempt risks doubling the run's wall time for a
// repo that is far more likely unreachable than briefly flaky. That check
// is structural (gitcmd wraps ErrTimeout) rather than a match on stderr
// text, which gitcmd's own documentation warns is not an API. No other
// classification is attempted: the errors worth distinguishing here fail
// in under a second, so retrying a 404 or a bad token costs nothing, while
// an stderr allowlist would silently stop retrying the moment a git or
// LibreSSL upgrade rephrased the message.
func ensureMirror(ctx context.Context, deps Deps, repo discover.Repo, fetchSem chan struct{}, budget *retryBudget) (retries int, err error) {
	for {
		fetchSem <- struct{}{}
		_, err = deps.Mirror.Ensure(ctx, repo, deps.Token)
		<-fetchSem
		if err == nil || errors.Is(err, gitcmd.ErrTimeout) {
			return retries, err
		}
		if retries > 0 || ctx.Err() != nil || !budget.take() {
			return retries, err
		}
		// The backoff is cancellable, and the counter is only incremented
		// once the retry is actually about to be attempted: a run canceled
		// mid-sleep never made a second attempt, so recording one would
		// overstate the flake activity the counter exists to measure.
		select {
		case <-ctx.Done():
			return retries, err
		case <-time.After(fetchRetryDelay):
		}
		retries++
	}
}

// syncRepo fetches and indexes one repo, returning its status. Any error is
// captured into RepoStatus.Error and the previously indexed commit retained,
// so a failure never loses the read_file pinning guarantee.
//
// now is passed in rather than read from deps because Run resolves the nil
// default into a local, so deps.Now is not safe to call here.
func syncRepo(ctx context.Context, deps Deps, ix Indexer, repo discover.Repo, prev status.RepoStatus, now func() time.Time, fetchSem, indexSem chan struct{}, budget *retryBudget) status.RepoStatus {
	// The per-repo freshness clock is carried forward with IndexedCommit:
	// a failed repo keeps serving its last good data, so it keeps the age
	// of that data rather than appearing never-synced.
	rs := status.RepoStatus{IndexedCommit: prev.IndexedCommit, LastSuccessAt: prev.LastSuccessAt}

	retries, err := ensureMirror(ctx, deps, repo, fetchSem, budget)
	// Recorded even when the retry ultimately succeeded: a repo that
	// needed one is the only evidence that the flake rate is climbing.
	rs.FetchRetries = retries
	if err != nil {
		rs.Error = status.TruncateError(err.Error())
		return rs
	}
	rs.Fetched = true

	dir := deps.Mirror.Dir(repo.FullName)
	head, err := deps.Mirror.HeadCommit(ctx, dir, repo.DefaultBranch)
	if err != nil {
		rs.Error = status.TruncateError(err.Error())
		return rs
	}

	// Invariant: IndexRepo indexes the branch tip, which equals the head we
	// just resolved only because no concurrent fetch of the same repo can
	// occur within a run.
	indexSem <- struct{}{}
	err = ix.IndexRepo(ctx, dir, repo.FullName, repo.DefaultBranch, head)
	<-indexSem
	if err != nil {
		rs.Error = status.TruncateError(err.Error())
		return rs
	}
	if err := deps.Mirror.MarkIndexed(ctx, dir, head); err != nil {
		rs.Error = status.TruncateError(err.Error())
		return rs
	}
	rs.Indexed = true
	rs.IndexedCommit = head
	rs.LastSuccessAt = now()
	// Housekeeping after a successful pin. A gc failure is recorded
	// separately from Error: the repo is fully synced either way, so it must
	// not fail the run, freeze the freshness clock, or trip the degraded
	// warning.
	if _, err := deps.Mirror.MaybeGC(ctx, dir); err != nil {
		rs.GCError = status.TruncateError(err.Error())
	}
	return rs
}
