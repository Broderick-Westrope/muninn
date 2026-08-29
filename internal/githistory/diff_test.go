package githistory

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	f := newFixtureRepo(t)

	// Patch defaults to false in single-rev mode: metadata plus stats.
	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Lockfile})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if d.Meta.SHA != f.Lockfile {
		t.Fatalf("Meta.SHA = %q, want %q", d.Meta.SHA, f.Lockfile)
	}
	if d.Meta.Author != "test" {
		t.Fatalf("Meta.Author = %q, want %q", d.Meta.Author, "test")
	}
	if d.Meta.AuthorDate != "2024-01-10" {
		t.Fatalf("Meta.AuthorDate = %q, want %q", d.Meta.AuthorDate, "2024-01-10")
	}
	if d.Meta.Message != "change Foo and lockfile" {
		t.Fatalf("Meta.Message = %q, want %q", d.Meta.Message, "change Foo and lockfile")
	}
	if len(d.Meta.Parents) != 1 {
		t.Fatalf("len(Meta.Parents) = %d, want 1", len(d.Meta.Parents))
	}
	if d.MergeBaseSHA != "" {
		t.Fatalf("MergeBaseSHA = %q, want empty", d.MergeBaseSHA)
	}
	if d.Warning != "" {
		t.Fatalf("Warning = %q, want empty", d.Warning)
	}

	if want := []string{"foo.go", "package-lock.json"}; !slices.Equal(filePaths(d.Files), want) {
		t.Fatalf("file paths = %v, want %v", filePaths(d.Files), want)
	}
	for _, file := range d.Files {
		if file.Patch != "" {
			t.Fatalf("%s Patch = %q, want empty", file.Path, file.Patch)
		}
		if file.StatLine == "" {
			t.Fatalf("%s StatLine is empty", file.Path)
		}
	}
	if d.Files[0].Generated {
		t.Fatal("foo.go Generated = true, want false")
	}
	if !d.Files[1].Generated {
		t.Fatal("package-lock.json Generated = false, want true")
	}
	if len(d.OmittedStats) != 0 {
		t.Fatalf("OmittedStats = %v, want empty", d.OmittedStats)
	}
}

func TestGetDiffSingleRevRootCommit(t *testing.T) {
	f := newFixtureRepo(t)

	// A root commit has no parent; the diff runs against the empty tree.
	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Root, Patch: boolPtr(true)})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if len(d.Meta.Parents) != 0 {
		t.Fatalf("Meta.Parents = %v, want empty", d.Meta.Parents)
	}
	if want := []string{"bar.txt", "doc.txt", "foo.go"}; !slices.Equal(filePaths(d.Files), want) {
		t.Fatalf("file paths = %v, want %v", filePaths(d.Files), want)
	}
	for _, file := range d.Files {
		if file.Patch == "" {
			t.Fatalf("%s Patch is empty", file.Path)
		}
	}
}

func TestGetDiffMergeCommitFirstParent(t *testing.T) {
	f := newFixtureRepo(t)

	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Merge, Patch: boolPtr(true)})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if len(d.Meta.Parents) != 2 {
		t.Fatalf("len(Meta.Parents) = %d, want 2", len(d.Meta.Parents))
	}

	// A bare `git show` would emit the (empty) combined format for this
	// clean merge; the first-parent diff must show Foo's change.
	if want := []string{"foo.go"}; !slices.Equal(filePaths(d.Files), want) {
		t.Fatalf("file paths = %v, want %v", filePaths(d.Files), want)
	}
	if !strings.Contains(d.Files[0].Patch, "+// Foo returns a number word.") {
		t.Fatalf("patch missing Foo's change:\n%s", d.Files[0].Patch)
	}

	want := fixGit(t, f.Mirror, nil, "diff", "--no-ext-diff", "--no-color", "--no-renames", f.Merge+"^1", f.Merge)
	if got := strings.TrimSpace(d.Files[0].Patch); got != want {
		t.Fatalf("patch = %q, want %q", got, want)
	}
}

func TestGetDiffPathFilter(t *testing.T) {
	f := newFixtureRepo(t)

	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Lockfile, Path: "foo.go", Patch: boolPtr(true)})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if want := []string{"foo.go"}; !slices.Equal(filePaths(d.Files), want) {
		t.Fatalf("file paths = %v, want %v", filePaths(d.Files), want)
	}
	if len(d.OmittedStats) != 0 {
		t.Fatalf("OmittedStats = %v, want empty", d.OmittedStats)
	}
}

func TestGetDiffTwoRevMergeBaseVsTwoDot(t *testing.T) {
	f := newFixtureRepo(t)
	ctx := context.Background()

	// Three-dot (default): only rev-side changes; div.txt never appears,
	// and the merge-base-differs warning fires.
	d, err := GetDiff(ctx, f.Mirror, DiffOptions{Rev: "main", Base: "divergent"})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if d.MergeBaseSHA != f.Root {
		t.Fatalf("MergeBaseSHA = %q, want root %q", d.MergeBaseSHA, f.Root)
	}
	if d.Behind != 1 {
		t.Fatalf("Behind = %d, want 1", d.Behind)
	}
	if d.Ahead <= 0 {
		t.Fatalf("Ahead = %d, want > 0", d.Ahead)
	}
	if slices.Contains(filePaths(d.Files), "div.txt") {
		t.Fatalf("file paths %v must not contain div.txt", filePaths(d.Files))
	}
	if !strings.Contains(d.Warning, "merge-base") {
		t.Fatalf("Warning = %q, want to contain %q", d.Warning, "merge-base")
	}
	if !strings.Contains(d.Warning, "merge_base: false") {
		t.Fatalf("Warning = %q, want to contain %q", d.Warning, "merge_base: false")
	}

	// Two-dot: point-to-point tree comparison, so div.txt shows up as a
	// deletion on the rev side.
	d, err = GetDiff(ctx, f.Mirror, DiffOptions{Rev: "main", Base: "divergent", MergeBase: boolPtr(false)})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	all := append(filePaths(d.Files), d.OmittedStats...)
	if !strings.Contains(strings.Join(all, "\n"), "div.txt") {
		t.Fatalf("two-dot diff missing div.txt: %v", all)
	}
}

func TestGetDiffDisjointHistoriesFallback(t *testing.T) {
	f := newFixtureRepo(t)

	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: "main", Base: "orphan"})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if d.MergeBaseSHA != "" {
		t.Fatalf("MergeBaseSHA = %q, want empty", d.MergeBaseSHA)
	}
	if !strings.Contains(d.Warning, "no merge base") {
		t.Fatalf("Warning = %q, want to contain %q", d.Warning, "no merge base")
	}
	all := append(filePaths(d.Files), d.OmittedStats...)
	if len(all) == 0 {
		t.Fatal("two-dot fallback must still diff")
	}
	if !strings.Contains(strings.Join(all, "\n"), "orphan.txt") {
		t.Fatalf("fallback diff missing orphan.txt: %v", all)
	}
}

func TestGetDiffDescendantAndSwappedWarnings(t *testing.T) {
	f := newFixtureRepo(t)
	ctx := context.Background()

	// Healthy direction: rev descends from base — no warning.
	d, err := GetDiff(ctx, f.Mirror, DiffOptions{Rev: "main", Base: f.Root})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if d.MergeBaseSHA != f.Root {
		t.Fatalf("MergeBaseSHA = %q, want root %q", d.MergeBaseSHA, f.Root)
	}
	if d.Warning != "" {
		t.Fatalf("Warning = %q, want empty", d.Warning)
	}
	if len(d.Files) == 0 {
		t.Fatal("Files is empty, want entries")
	}

	// Swapped endpoints: rev is an ancestor of base, so the three-dot
	// diff is silently empty — the warning must say so. The merge_base:
	// false advice arrives from both resolveEndpoints and the empty-diff
	// check; it must be deduplicated to a single occurrence.
	d, err = GetDiff(ctx, f.Mirror, DiffOptions{Rev: f.Root, Base: "main"})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if len(d.Files) != 0 {
		t.Fatalf("Files = %v, want empty", filePaths(d.Files))
	}
	if len(d.OmittedStats) != 0 {
		t.Fatalf("OmittedStats = %v, want empty", d.OmittedStats)
	}
	if !strings.Contains(d.Warning, "empty") {
		t.Fatalf("Warning = %q, want to contain %q", d.Warning, "empty")
	}
	if !strings.Contains(d.Warning, "swapped") {
		t.Fatalf("Warning = %q, want to contain %q", d.Warning, "swapped")
	}
	if got := strings.Count(d.Warning, "use merge_base: false for a point-to-point comparison"); got != 1 {
		t.Fatalf("merge_base advice occurs %d times in warning, want 1: %q", got, d.Warning)
	}
}

func TestGetDiffIdenticalEndpoints(t *testing.T) {
	f := newFixtureRepo(t)

	// base == rev: the diff is trivially empty, but the endpoints are
	// not swapped — the warning must say "same commit" instead.
	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Root, Base: f.Root})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if len(d.Files) != 0 {
		t.Fatalf("Files = %v, want empty", filePaths(d.Files))
	}
	if len(d.OmittedStats) != 0 {
		t.Fatalf("OmittedStats = %v, want empty", d.OmittedStats)
	}
	if !strings.Contains(d.Warning, "empty") {
		t.Fatalf("Warning = %q, want to contain %q", d.Warning, "empty")
	}
	if !strings.Contains(d.Warning, "same commit") {
		t.Fatalf("Warning = %q, want to contain %q", d.Warning, "same commit")
	}
	if strings.Contains(d.Warning, "swapped") {
		t.Fatalf("Warning = %q, must not contain %q", d.Warning, "swapped")
	}
}

func TestGetDiffLockfileStatOnly(t *testing.T) {
	f := newFixtureRepo(t)
	ctx := context.Background()

	d, err := GetDiff(ctx, f.Mirror, DiffOptions{Rev: f.Lockfile, Patch: boolPtr(true)})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}

	// The source patch stays intact; the lockfile is a stat line.
	if want := []string{"foo.go"}; !slices.Equal(filePaths(d.Files), want) {
		t.Fatalf("file paths = %v, want %v", filePaths(d.Files), want)
	}
	if !strings.Contains(d.Files[0].Patch, "-\treturn \"one\"") {
		t.Fatalf("patch missing removed line:\n%s", d.Files[0].Patch)
	}
	if !strings.Contains(d.Files[0].Patch, "+\treturn \"two\"") {
		t.Fatalf("patch missing added line:\n%s", d.Files[0].Patch)
	}
	if len(d.OmittedStats) != 1 {
		t.Fatalf("len(OmittedStats) = %d, want 1: %v", len(d.OmittedStats), d.OmittedStats)
	}
	if !strings.Contains(d.OmittedStats[0], "package-lock.json") {
		t.Fatalf("OmittedStats[0] = %q, want to contain %q", d.OmittedStats[0], "package-lock.json")
	}
	if !strings.Contains(d.OmittedStats[0], "generated") {
		t.Fatalf("OmittedStats[0] = %q, want to contain %q", d.OmittedStats[0], "generated")
	}

	// include_generated restores the lockfile patch.
	d, err = GetDiff(ctx, f.Mirror, DiffOptions{Rev: f.Lockfile, Patch: boolPtr(true), IncludeGenerated: boolPtr(true)})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if want := []string{"foo.go", "package-lock.json"}; !slices.Equal(filePaths(d.Files), want) {
		t.Fatalf("file paths = %v, want %v", filePaths(d.Files), want)
	}
	if !d.Files[1].Generated {
		t.Fatal("package-lock.json Generated = false, want true")
	}
	if d.Files[1].Patch == "" {
		t.Fatal("package-lock.json Patch is empty")
	}
	if len(d.OmittedStats) != 0 {
		t.Fatalf("OmittedStats = %v, want empty", d.OmittedStats)
	}
}

func TestGetDiffOverBudgetSingleFile(t *testing.T) {
	f := newFixtureRepo(t)

	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Big, Patch: boolPtr(true)})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if len(d.Files) != 0 {
		t.Fatalf("Files = %v, want empty", filePaths(d.Files))
	}
	if len(d.OmittedStats) != 1 {
		t.Fatalf("len(OmittedStats) = %d, want 1: %v", len(d.OmittedStats), d.OmittedStats)
	}
	if !strings.Contains(d.OmittedStats[0], "big.txt") {
		t.Fatalf("OmittedStats[0] = %q, want to contain %q", d.OmittedStats[0], "big.txt")
	}
	if !strings.Contains(d.OmittedStats[0], "budget") {
		t.Fatalf("OmittedStats[0] = %q, want to contain %q", d.OmittedStats[0], "budget")
	}
}

func TestGetDiffBinaryStatOnly(t *testing.T) {
	f := newFixtureRepo(t)

	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Binary, Patch: boolPtr(true)})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if len(d.Files) != 0 {
		t.Fatalf("Files = %v, want empty", filePaths(d.Files))
	}
	if want := []string{"bin.dat | binary"}; !slices.Equal(d.OmittedStats, want) {
		t.Fatalf("OmittedStats = %v, want %v", d.OmittedStats, want)
	}
}

func TestGetDiffTruncationKeepsPatchesWhole(t *testing.T) {
	f := newFixtureRepo(t)

	// The Mixed commit rewrites big.txt (over budget) and adds small.txt
	// (fits): truncation must omit the former and keep the latter whole.
	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.Mixed, Patch: boolPtr(true)})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if want := []string{"small.txt"}; !slices.Equal(filePaths(d.Files), want) {
		t.Fatalf("file paths = %v, want %v", filePaths(d.Files), want)
	}
	if len(d.OmittedStats) != 1 {
		t.Fatalf("len(OmittedStats) = %d, want 1: %v", len(d.OmittedStats), d.OmittedStats)
	}
	if !strings.Contains(d.OmittedStats[0], "big.txt") {
		t.Fatalf("OmittedStats[0] = %q, want to contain %q", d.OmittedStats[0], "big.txt")
	}

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
		if err != nil {
			t.Fatalf("git apply --check %s: %v\n%s", file.Path, err, out)
		}
	}
}

func TestGetDiffNonASCIIPath(t *testing.T) {
	f := newFixtureRepo(t)

	// core.quotepath (default true) C-quotes non-ASCII names in --patch
	// headers while --numstat -z emits them raw; order matching must
	// still pair the patch with its stat entry.
	d, err := GetDiff(context.Background(), f.Mirror, DiffOptions{Rev: f.NonASCII, Patch: boolPtr(true)})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if want := []string{"café.txt"}; !slices.Equal(filePaths(d.Files), want) {
		t.Fatalf("file paths = %v, want %v", filePaths(d.Files), want)
	}
	if !strings.Contains(d.Files[0].Patch, "+café v1") {
		t.Fatalf("patch missing added line:\n%s", d.Files[0].Patch)
	}
	if len(d.OmittedStats) != 0 {
		t.Fatalf("OmittedStats = %v, want empty", d.OmittedStats)
	}
}

func TestGetDiffValidation(t *testing.T) {
	f := newFixtureRepo(t)
	ctx := context.Background()

	_, err := GetDiff(ctx, f.Mirror, DiffOptions{Rev: "main", Path: "-p"})
	if err == nil || !strings.Contains(err.Error(), "must not start with") {
		t.Fatalf("err = %v, want to contain %q", err, "must not start with")
	}

	if _, err = GetDiff(ctx, f.Mirror, DiffOptions{Rev: "no-such-branch"}); err == nil {
		t.Fatal("err = nil, want error for unknown branch")
	}
}
