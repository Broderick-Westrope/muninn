// Package index wraps Zoekt's git indexer behind a small API. Zoekt types
// are an implementation detail and never appear in this package's public
// signatures (spec decision).
package index

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/gitindex"
	zoektindex "github.com/sourcegraph/zoekt/index"
)

// defaultSizeMax is the per-file size cap applied when SizeMax is zero.
const defaultSizeMax = 1 << 20 // 1 MiB

// stagingPrefix names the per-build directories IndexRepo creates inside
// IndexDir. They are hidden and nested, so neither Zoekt's directory
// searcher nor ListIndexed (both top-level *.zoekt globs) ever sees a staged
// shard.
const stagingPrefix = ".staging-"

// Indexer builds and manages Zoekt shards for bare git mirrors.
type Indexer struct {
	// IndexDir is the directory holding *.zoekt shard files.
	IndexDir string
	// CtagsPath is the universal-ctags binary used for symbol extraction.
	// Empty disables ctags entirely (shards are still built, without
	// symbols); it is always set explicitly so Zoekt never probes PATH.
	CtagsPath string
	// SizeMax is the maximum file size to index; 0 means defaultSizeMax.
	SizeMax int
}

// buildOptions returns the Zoekt build options for a repo. The same options
// must be used for indexing and shard discovery so shard names line up.
func (ix *Indexer) buildOptions(fullName string) zoektindex.Options {
	return ix.buildOptionsIn(ix.IndexDir, fullName)
}

// buildOptionsIn is buildOptions writing to dir instead of IndexDir. Shard
// names do not depend on the directory, so a shard staged in dir keeps its
// name when published.
func (ix *Indexer) buildOptionsIn(dir, fullName string) zoektindex.Options {
	sizeMax := ix.SizeMax
	if sizeMax == 0 {
		sizeMax = defaultSizeMax
	}
	return zoektindex.Options{
		IndexDir: dir,
		SizeMax:  sizeMax,
		// CTagsPath is set explicitly; if empty we disable ctags so that
		// Options.SetDefaults (called inside IndexGitRepo) does not probe
		// PATH for a binary. ScipCTagsPath is deliberately left empty: with
		// an empty LanguageMap Zoekt routes every language to
		// universal-ctags, so scip-ctags is never invoked.
		CTagsPath:    ix.CtagsPath,
		DisableCTags: ix.CtagsPath == "",
		RepositoryDescription: zoekt.Repository{
			Name: fullName,
		},
	}
}

// IndexRepo indexes the tip of branch in the bare mirror at mirrorDir into
// shards named after fullName.
//
// Zoekt indexes the current tip of the branch, not a caller-chosen commit;
// the commit argument matches that tip only because sync never fetches the
// same repo concurrently within a run (invariant). With Incremental set,
// gitindex.IndexGitRepo compares the branch tips recorded in the existing
// shard's metadata (plus an options hash) against the repo and returns
// without rebuilding when they are equal (Options.IncrementalSkipIndexing),
// which is what makes repeat syncs cheap.
//
// The build runs in a staging directory and is published only if it
// succeeds, so a failed build leaves the live shards exactly as they were.
// Zoekt alone does not guarantee this: IndexGitRepo defers Builder.Finish,
// and Finish discards its output only when writing a shard failed. An error
// reading the repository (an unreadable blob, say) returns early, and the
// deferred Finish then publishes the files added so far as the repo's shard
// and deletes the complete one it replaces. The partial shard also records
// the new branch tip, so every later incremental run skips the repo as
// already indexed. The staging directory is seeded with hard links to the
// live shards so that incremental check still sees them.
//
// Limitation: gitindex.IndexGitRepo does not accept a context, so ctx is
// only checked for cancellation before indexing starts; an in-flight build
// cannot be interrupted.
func (ix *Indexer) IndexRepo(ctx context.Context, mirrorDir, fullName, branch, commit string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("indexing %s: %w", fullName, err)
	}
	_ = commit // recorded by the caller; see invariant above

	if err := os.MkdirAll(ix.IndexDir, 0o700); err != nil {
		return fmt.Errorf("creating index dir %s: %w", ix.IndexDir, err)
	}
	staging, err := os.MkdirTemp(ix.IndexDir, stagingPrefix+"*")
	if err != nil {
		return fmt.Errorf("creating staging dir for %s: %w", fullName, err)
	}
	defer os.RemoveAll(staging)

	liveOpts := ix.buildOptions(fullName)
	live := liveOpts.FindAllShards()
	for _, shard := range live {
		if err := linkShardInto(shard, staging); err != nil {
			return fmt.Errorf("staging live shards of %s: %w", fullName, err)
		}
	}

	stagedOpts := ix.buildOptionsIn(staging, fullName)
	opts := gitindex.Options{
		RepoDir:      mirrorDir,
		Incremental:  true,
		Branches:     []string{branch},
		BuildOptions: stagedOpts,
	}
	updated, err := gitindex.IndexGitRepo(opts)
	if err != nil {
		return fmt.Errorf("indexing %s (branch %s): %w", fullName, branch, err)
	}
	if !updated {
		return nil
	}
	if err := ix.publish(stagedOpts.FindAllShards(), live); err != nil {
		return fmt.Errorf("publishing shards of %s: %w", fullName, err)
	}
	return nil
}

// linkShardInto hard-links a shard and its .meta sidecar into dir under the
// same names.
func linkShardInto(shard, dir string) error {
	paths, err := zoektindex.IndexFilePaths(shard)
	if err != nil {
		return fmt.Errorf("listing files for shard %s: %w", shard, err)
	}
	for _, p := range paths {
		if err := os.Link(p, filepath.Join(dir, filepath.Base(p))); err != nil {
			return fmt.Errorf("linking %s: %w", p, err)
		}
	}
	return nil
}

// publish moves a successful build's shards from the staging directory into
// IndexDir, then removes the live shards the new build no longer has (a repo
// that shrank below a shard boundary). Each rename atomically replaces the
// shard of the same name, so a searcher sees the old shard or the new one,
// never a partial file.
func (ix *Indexer) publish(staged, live []string) error {
	keep := make(map[string]bool, len(staged))
	for _, shard := range staged {
		paths, err := zoektindex.IndexFilePaths(shard)
		if err != nil {
			return fmt.Errorf("listing files for staged shard %s: %w", shard, err)
		}
		for _, p := range paths {
			if err := os.Rename(p, filepath.Join(ix.IndexDir, filepath.Base(p))); err != nil {
				return fmt.Errorf("moving %s into place: %w", p, err)
			}
		}
		keep[filepath.Base(shard)] = true
	}
	for _, shard := range live {
		if keep[filepath.Base(shard)] {
			continue
		}
		for _, p := range []string{shard, shard + ".meta"} {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("removing stale shard file %s: %w", p, err)
			}
		}
	}
	return nil
}

// CleanTmp removes leftover *.tmp files and staging directories in IndexDir
// (crash recovery: Zoekt writes shards to a .tmp file before renaming into
// place, and IndexRepo builds in a staging directory it removes on return).
func (ix *Indexer) CleanTmp() error {
	tmps, err := filepath.Glob(filepath.Join(ix.IndexDir, "*.tmp"))
	if err != nil {
		return fmt.Errorf("globbing tmp files in %s: %w", ix.IndexDir, err)
	}
	for _, tmp := range tmps {
		if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing tmp file %s: %w", tmp, err)
		}
	}
	stagings, err := filepath.Glob(filepath.Join(ix.IndexDir, stagingPrefix+"*"))
	if err != nil {
		return fmt.Errorf("globbing staging dirs in %s: %w", ix.IndexDir, err)
	}
	for _, dir := range stagings {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("removing staging dir %s: %w", dir, err)
		}
	}
	return nil
}

// RemoveShards deletes all shard files (and their .meta sidecars) belonging
// to the repo. Shard discovery uses Zoekt's own naming via
// Options.FindAllShards rather than replicating the name scheme. Removing a
// repo that has no shards is a no-op.
func (ix *Indexer) RemoveShards(fullName string) error {
	opts := ix.buildOptions(fullName)
	for _, shard := range opts.FindAllShards() {
		paths, err := zoektindex.IndexFilePaths(shard)
		if err != nil {
			return fmt.Errorf("listing files for shard %s: %w", shard, err)
		}
		for _, p := range paths {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("removing shard file %s: %w", p, err)
			}
		}
	}
	return nil
}

// ListIndexed returns repo full name → indexed commit SHA for every shard in
// IndexDir, read from shard metadata (each shard records the branch tip it
// was built from in Repository.Branches[].Version). Used as a cross-check
// against the status file.
func (ix *Indexer) ListIndexed() (map[string]string, error) {
	shards, err := filepath.Glob(filepath.Join(ix.IndexDir, "*.zoekt"))
	if err != nil {
		return nil, fmt.Errorf("globbing shards in %s: %w", ix.IndexDir, err)
	}
	indexed := make(map[string]string)
	for _, shard := range shards {
		repos, _, err := zoektindex.ReadMetadataPathAlive(shard)
		if err != nil {
			return nil, fmt.Errorf("reading metadata of shard %s: %w", shard, err)
		}
		for _, repo := range repos {
			if len(repo.Branches) > 0 {
				indexed[repo.Name] = repo.Branches[0].Version
			}
		}
	}
	return indexed, nil
}
