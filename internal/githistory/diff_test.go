package githistory

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// filePaths extracts the Path column of the emitted file entries.
func filePaths(files []FileDiff) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

func TestGetDiffSingleRevMetadataAndStat(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	// Patch defaults to false in single-rev mode: metadata plus stats.
	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Lockfile})
	require.NoError(t, err)
	require.Equal(t, f.Lockfile, d.Meta.SHA)
	require.Equal(t, "test", d.Meta.Author)
	require.Equal(t, "2024-01-10", d.Meta.AuthorDate)
	require.Equal(t, "change Foo and lockfile", d.Meta.Message)
	require.Len(t, d.Meta.Parents, 1)
	require.Empty(t, d.MergeBaseSHA)
	require.Empty(t, d.Warning)

	require.Equal(t, []string{"foo.go", "package-lock.json"}, filePaths(d.Files))
	for _, file := range d.Files {
		require.Empty(t, file.Patch)
		require.NotEmpty(t, file.StatLine)
	}
	require.False(t, d.Files[0].Generated)
	require.True(t, d.Files[1].Generated)
	require.Empty(t, d.OmittedStats)
}

func TestGetDiffSingleRevRootCommit(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	// A root commit has no parent; the diff runs against the empty tree.
	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Root, Patch: boolPtr(true)})
	require.NoError(t, err)
	require.Empty(t, d.Meta.Parents)
	require.Equal(t, []string{"bar.txt", "doc.txt", "foo.go"}, filePaths(d.Files))
	for _, file := range d.Files {
		require.NotEmpty(t, file.Patch)
	}
}

func TestGetDiffMergeCommitFirstParent(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Merge, Patch: boolPtr(true)})
	require.NoError(t, err)
	require.Len(t, d.Meta.Parents, 2)

	// A bare `git show` would emit the (empty) combined format for this
	// clean merge; the first-parent diff must show Foo's change.
	require.Equal(t, []string{"foo.go"}, filePaths(d.Files))
	require.Contains(t, d.Files[0].Patch, "+// Foo returns a number word.")

	want := fixGit(t, f.Mirror, nil, "diff", "--no-ext-diff", "--no-color", "--no-renames", f.Merge+"^1", f.Merge)
	require.Equal(t, want, strings.TrimSpace(d.Files[0].Patch))
}

func TestGetDiffPathFilter(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Lockfile, Path: "foo.go", Patch: boolPtr(true)})
	require.NoError(t, err)
	require.Equal(t, []string{"foo.go"}, filePaths(d.Files))
	require.Empty(t, d.OmittedStats)
}

func TestGetDiffTwoRevMergeBaseVsTwoDot(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)
	ctx := context.Background()

	// Three-dot (default): only rev-side changes; div.txt never appears,
	// and the merge-base-differs warning fires.
	d, err := GetDiff(ctx, f.Mirror, DiffOptions{Rev: "main", Base: "divergent"})
	require.NoError(t, err)
	require.Equal(t, f.Root, d.MergeBaseSHA)
	require.Equal(t, 1, d.Behind)
	require.Positive(t, d.Ahead)
	require.NotContains(t, filePaths(d.Files), "div.txt")
	require.Contains(t, d.Warning, "merge-base")
	require.Contains(t, d.Warning, "merge_base: false")

	// Two-dot: point-to-point tree comparison, so div.txt shows up as a
	// deletion on the rev side.
	d, err = GetDiff(ctx, f.Mirror, DiffOptions{Rev: "main", Base: "divergent", MergeBase: boolPtr(false)})
	require.NoError(t, err)
	all := append(filePaths(d.Files), d.OmittedStats...)
	require.Contains(t, strings.Join(all, "\n"), "div.txt")
}

func TestGetDiffDisjointHistoriesFallback(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: "main", Base: "orphan"})
	require.NoError(t, err)
	require.Empty(t, d.MergeBaseSHA)
	require.Contains(t, d.Warning, "no merge base")
	all := append(filePaths(d.Files), d.OmittedStats...)
	require.NotEmpty(t, all, "two-dot fallback must still diff")
	require.Contains(t, strings.Join(all, "\n"), "orphan.txt")
}

func TestGetDiffDescendantAndSwappedWarnings(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)
	ctx := context.Background()

	// Healthy direction: rev descends from base — no warning.
	d, err := GetDiff(ctx, f.Mirror, DiffOptions{Rev: "main", Base: f.Root})
	require.NoError(t, err)
	require.Equal(t, f.Root, d.MergeBaseSHA)
	require.Empty(t, d.Warning)
	require.NotEmpty(t, d.Files)

	// Swapped endpoints: rev is an ancestor of base, so the three-dot
	// diff is silently empty — the warning must say so. The merge_base:
	// false advice arrives from both resolveEndpoints and the empty-diff
	// check; it must be deduplicated to a single occurrence.
	d, err = GetDiff(ctx, f.Mirror, DiffOptions{Rev: f.Root, Base: "main"})
	require.NoError(t, err)
	require.Empty(t, d.Files)
	require.Empty(t, d.OmittedStats)
	require.Contains(t, d.Warning, "empty")
	require.Contains(t, d.Warning, "swapped")
	require.Equal(t, 1, strings.Count(d.Warning, "use merge_base: false for a point-to-point comparison"))
}

func TestGetDiffIdenticalEndpoints(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	// base == rev: the diff is trivially empty, but the endpoints are
	// not swapped — the warning must say "same commit" instead.
	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Root, Base: f.Root})
	require.NoError(t, err)
	require.Empty(t, d.Files)
	require.Empty(t, d.OmittedStats)
	require.Contains(t, d.Warning, "empty")
	require.Contains(t, d.Warning, "same commit")
	require.NotContains(t, d.Warning, "swapped")
}

func TestGetDiffLockfileStatOnly(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)
	ctx := context.Background()

	d, err := GetDiff(ctx, f.Mirror, DiffOptions{Rev: f.Lockfile, Patch: boolPtr(true)})
	require.NoError(t, err)

	// The source patch stays intact; the lockfile is a stat line.
	require.Equal(t, []string{"foo.go"}, filePaths(d.Files))
	require.Contains(t, d.Files[0].Patch, `-	return "one"`)
	require.Contains(t, d.Files[0].Patch, `+	return "two"`)
	require.Len(t, d.OmittedStats, 1)
	require.Contains(t, d.OmittedStats[0], "package-lock.json")
	require.Contains(t, d.OmittedStats[0], "generated")

	// include_generated restores the lockfile patch.
	d, err = GetDiff(ctx, f.Mirror, DiffOptions{Rev: f.Lockfile, Patch: boolPtr(true), IncludeGenerated: boolPtr(true)})
	require.NoError(t, err)
	require.Equal(t, []string{"foo.go", "package-lock.json"}, filePaths(d.Files))
	require.True(t, d.Files[1].Generated)
	require.NotEmpty(t, d.Files[1].Patch)
	require.Empty(t, d.OmittedStats)
}

func TestGetDiffOverBudgetSingleFile(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Big, Patch: boolPtr(true)})
	require.NoError(t, err)
	require.Empty(t, d.Files)
	require.Len(t, d.OmittedStats, 1)
	require.Contains(t, d.OmittedStats[0], "big.txt")
	require.Contains(t, d.OmittedStats[0], "budget")
}

func TestGetDiffBinaryStatOnly(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Binary, Patch: boolPtr(true)})
	require.NoError(t, err)
	require.Empty(t, d.Files)
	require.Equal(t, []string{"bin.dat | binary"}, d.OmittedStats)
}

func TestGetDiffTruncationKeepsPatchesWhole(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	// The Mixed commit rewrites big.txt (over budget) and adds small.txt
	// (fits): truncation must omit the former and keep the latter whole.
	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Mixed, Patch: boolPtr(true)})
	require.NoError(t, err)
	require.Equal(t, []string{"small.txt"}, filePaths(d.Files))
	require.Len(t, d.OmittedStats, 1)
	require.Contains(t, d.OmittedStats[0], "big.txt")

	// Every emitted patch must apply cleanly at the pre-image rev: a
	// plain-path clone copies the whole object store (a file:// URL would
	// pack only reachable objects).
	scratch := filepath.Join(t.TempDir(), "scratch")
	fixGit(t, "", nil, "clone", f.Mirror, scratch)
	fixGit(t, scratch, nil, "checkout", f.Big)
	for _, file := range d.Files {
		cmd := exec.Command("git", "-C", scratch, "apply", "--check")
		cmd.Stdin = strings.NewReader(file.Patch)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git apply --check %s: %s", file.Path, out)
	}
}

func TestGetDiffNonASCIIPath(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	// core.quotepath (default true) C-quotes non-ASCII names in --patch
	// headers while --numstat -z emits them raw; order matching must
	// still pair the patch with its stat entry.
	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.NonASCII, Patch: boolPtr(true)})
	require.NoError(t, err)
	require.Equal(t, []string{"café.txt"}, filePaths(d.Files))
	require.Contains(t, d.Files[0].Patch, "+café v1")
	require.Empty(t, d.OmittedStats)
}

func TestGetDiffValidation(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)
	ctx := context.Background()

	_, err := GetDiff(ctx, f.Mirror, DiffOptions{Rev: "main", Path: "-p"})
	require.ErrorContains(t, err, "must not start with")

	_, err = GetDiff(ctx, f.Mirror, DiffOptions{Rev: "no-such-branch"})
	require.Error(t, err)
}
