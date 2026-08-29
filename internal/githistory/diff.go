package githistory

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/broderick-westrope/muninn/internal/gitcmd"
	"github.com/broderick-westrope/muninn/internal/gitfile"
)

// diffByteBudget is the total byte budget across all rendered patch
// sections; whole per-file patches are appended while they fit, never a
// partial hunk.
const diffByteBudget = 64 << 10

// emptyTreeSHA is git's well-known empty tree object, used as the diff
// base for root commits (git hash-object -t tree /dev/null).
const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// generatedPatterns are basename globs for generated and lockfile paths
// that are stat-only by default.
var generatedPatterns = []string{
	"package-lock.json",
	"yarn.lock",
	"pnpm-lock.yaml",
	"go.sum",
	"*.pb.go",
	"Cargo.lock",
}

// DiffOptions selects what GetDiff shows. Rev alone shows that commit;
// Rev plus Base compares from base to rev.
type DiffOptions struct {
	// Rev is the commit to show, or the right-hand endpoint.
	Rev string
	// Base is the left-hand endpoint; empty selects single-rev mode.
	Base string
	// Path restricts the diff to one literal path.
	Path string
	// Patch includes per-file patches; nil defaults to false in
	// single-rev mode and true in two-rev mode.
	Patch *bool
	// MergeBase selects three-dot semantics in two-rev mode (diff from
	// the merge base of base and rev); nil defaults to true.
	MergeBase *bool
	// StatOnly suppresses patches entirely; nil defaults to false.
	StatOnly *bool
	// IncludeGenerated includes patches for generated/lockfile paths;
	// nil defaults to false.
	IncludeGenerated *bool
}

// CommitMeta is the metadata of the commit a diff describes.
type CommitMeta struct {
	// SHA is the full commit SHA.
	SHA string
	// Author is the author name.
	Author string
	// AuthorDate is the author date in YYYY-MM-DD form.
	AuthorDate string
	// Message is the full commit message.
	Message string
	// Parents are the parent commit SHAs; empty for root commits.
	Parents []string
}

// FileDiff is one file's rendered diff entry.
type FileDiff struct {
	// Path is the file path relative to the repo root.
	Path string
	// Patch is the whole per-file patch including its diff --git header;
	// empty in stat-only rendering.
	Patch string
	// StatLine is a one-line change summary for the file.
	StatLine string
	// Binary reports a binary file (always stat-only).
	Binary bool
	// Generated reports a generated/lockfile path.
	Generated bool
}

// Diff is the result of GetDiff.
type Diff struct {
	// Meta describes the rev endpoint's commit.
	Meta CommitMeta
	// BaseSHA is the resolved base commit in two-rev mode; empty in
	// single-rev mode.
	BaseSHA string
	// MergeBaseSHA is the computed merge base in two-rev mode; empty in
	// single-rev mode or for disjoint histories.
	MergeBaseSHA string
	// Ahead is how many commits rev has that base does not (two-rev
	// mode).
	Ahead int
	// Behind is how many commits base has that rev does not (two-rev
	// mode).
	Behind int
	// Files are the emitted per-file entries, in git's diff order.
	Files []FileDiff
	// OmittedStats are stat lines (with notices) for files whose patches
	// were withheld: generated, binary, or over the byte budget.
	OmittedStats []string
	// Warning flags empty diffs and merge-base surprises so agents never
	// mistake a silently empty diff for "no changes".
	Warning string
}

// GetDiff shows one commit (Rev alone) or compares two revs (from Base to
// Rev) in the bare mirror at mirrorDir. Merge commits are diffed against
// their first parent — a bare git show would emit the combined format,
// which is empty for clean merges. Patches are truncated at file
// boundaries under a total byte budget, never mid-hunk.
func GetDiff(ctx context.Context, mirrorDir string, opts DiffOptions) (*Diff, error) {
	if err := validatePath(opts.Path); err != nil {
		return nil, err
	}
	revSHA, err := gitfile.ResolveRev(ctx, mirrorDir, opts.Rev)
	if err != nil {
		return nil, err
	}
	meta, err := commitMeta(ctx, mirrorDir, revSHA)
	if err != nil {
		return nil, err
	}

	twoRev := opts.Base != ""
	d := &Diff{Meta: meta}
	var from string
	switch {
	case !twoRev:
		switch len(meta.Parents) {
		case 0:
			// Root commit: diff against the empty tree.
			from = emptyTreeSHA
		default:
			// Merge commits get the first-parent diff explicitly; for
			// non-merges rev^1 is simply rev^.
			from = meta.Parents[0]
		}
	default:
		baseSHA, err := gitfile.ResolveRev(ctx, mirrorDir, opts.Base)
		if err != nil {
			return nil, err
		}
		d.BaseSHA = baseSHA
		from, err = resolveEndpoints(ctx, mirrorDir, d, baseSHA, revSHA, opts.MergeBase == nil || *opts.MergeBase)
		if err != nil {
			return nil, err
		}
	}

	patch := twoRev
	if opts.Patch != nil {
		patch = *opts.Patch
	}
	if opts.StatOnly != nil && *opts.StatOnly {
		patch = false
	}
	includeGenerated := opts.IncludeGenerated != nil && *opts.IncludeGenerated

	if err := populateFiles(ctx, mirrorDir, d, from, revSHA, opts.Path, patch, includeGenerated); err != nil {
		return nil, err
	}
	if twoRev && len(d.Files) == 0 && len(d.OmittedStats) == 0 {
		emptyWarning := fmt.Sprintf("diff from %s to %s is empty", shortSHA(from), shortSHA(revSHA))
		if d.MergeBaseSHA == revSHA {
			emptyWarning += " because rev is an ancestor of base — the arguments may be swapped, or use merge_base: false for a point-to-point comparison"
		}
		if d.Warning != "" {
			d.Warning += "; "
		}
		d.Warning += emptyWarning
	}
	return d, nil
}

// resolveEndpoints computes the two-rev diff's left endpoint plus the
// merge base, ahead/behind counts, and merge-base warnings.
func resolveEndpoints(ctx context.Context, mirrorDir string, d *Diff, baseSHA, revSHA string, useMergeBase bool) (from string, err error) {
	run := runner(diffTimeout)
	mb, err := run.Run(ctx, "-C", mirrorDir, "merge-base", "--end-of-options", baseSHA, revSHA)
	if err != nil {
		// Exit code 1 with empty stdout is a legitimate "no merge base"
		// answer: the histories are disjoint.
		var gitErr *gitcmd.Error
		if !errors.As(err, &gitErr) || gitErr.ExitCode != 1 || mb != "" {
			return "", fmt.Errorf("computing merge base of %s and %s: %w", shortSHA(baseSHA), shortSHA(revSHA), err)
		}
		d.Warning = fmt.Sprintf("no merge base between %s and %s (disjoint histories); showing the two-dot diff %s..%s",
			shortSHA(baseSHA), shortSHA(revSHA), shortSHA(baseSHA), shortSHA(revSHA))
		mb = ""
	}
	d.MergeBaseSHA = mb

	counts, err := run.Run(ctx, "-C", mirrorDir, "rev-list", "--count", "--left-right", "--end-of-options", baseSHA+"..."+revSHA)
	if err != nil {
		return "", fmt.Errorf("counting commits between %s and %s: %w", shortSHA(baseSHA), shortSHA(revSHA), err)
	}
	left, right, ok := strings.Cut(counts, "\t")
	if !ok {
		return "", fmt.Errorf("malformed rev-list --left-right output %q", counts)
	}
	if d.Behind, err = strconv.Atoi(left); err != nil {
		return "", fmt.Errorf("parsing rev-list count %q: %w", counts, err)
	}
	if d.Ahead, err = strconv.Atoi(right); err != nil {
		return "", fmt.Errorf("parsing rev-list count %q: %w", counts, err)
	}

	from = baseSHA
	if useMergeBase && mb != "" {
		from = mb
		if mb != baseSHA {
			d.Warning = fmt.Sprintf("merge-base %s differs from base %s: rev is %d commits ahead, base is %d commits ahead; the three-dot diff shows only rev-side changes — use merge_base: false for a point-to-point comparison",
				shortSHA(mb), shortSHA(baseSHA), d.Ahead, d.Behind)
		}
	}
	return from, nil
}

// commitMeta reads a commit's metadata via git show --no-patch. Fields are
// separated by the unit separator (%x1f) so the free-form message stays
// parseable.
func commitMeta(ctx context.Context, mirrorDir, sha string) (CommitMeta, error) {
	out, err := runner(diffTimeout).Run(ctx, "-C", mirrorDir, "show", "--no-patch",
		"--format=%H%x1f%an%x1f%as%x1f%P%x1f%B", "--end-of-options", sha)
	if err != nil {
		return CommitMeta{}, fmt.Errorf("reading metadata of %s: %w", shortSHA(sha), err)
	}
	fields := strings.SplitN(out, "\x1f", 5)
	if len(fields) != 5 {
		return CommitMeta{}, fmt.Errorf("malformed show output for %s: %q", shortSHA(sha), out)
	}
	return CommitMeta{
		SHA:        fields[0],
		Author:     fields[1],
		AuthorDate: fields[2],
		Parents:    strings.Fields(fields[3]),
		Message:    strings.TrimSpace(fields[4]),
	}, nil
}

// populateFiles fills Files and OmittedStats for the diff from..to. The
// file list and stats come from a --numstat pre-pass; patches come from a
// single --patch invocation split at file boundaries. Renames are
// disabled (--no-renames) so every numstat record names exactly one path.
func populateFiles(ctx context.Context, mirrorDir string, d *Diff, from, to, pathFilter string, patch, includeGenerated bool) error {
	run := runner(diffTimeout)
	diffArgs := func(modes ...string) []string {
		args := []string{"-C", mirrorDir, "diff", "--no-ext-diff", "--no-color", "--no-renames"}
		args = append(args, modes...)
		args = append(args, "--end-of-options", from, to)
		if pathFilter != "" {
			args = append(args, "--", pathFilter)
		}
		return args
	}

	numstat, err := run.RunRaw(ctx, diffArgs("--numstat", "-z")...)
	if err != nil {
		return diffErr(from, to, err)
	}
	type statEntry struct {
		path     string
		statLine string
		binary   bool
	}
	var stats []statEntry
	for _, record := range strings.Split(numstat, "\x00") {
		if record == "" {
			continue
		}
		fields := strings.SplitN(record, "\t", 3)
		if len(fields) != 3 {
			return fmt.Errorf("malformed numstat record %q", record)
		}
		entry := statEntry{path: fields[2], binary: fields[0] == "-"}
		if entry.binary {
			entry.statLine = entry.path + " | binary"
		} else {
			entry.statLine = fmt.Sprintf("%s | +%s -%s", entry.path, fields[0], fields[1])
		}
		stats = append(stats, entry)
	}
	if len(stats) == 0 {
		return nil
	}

	var sections []string
	if patch {
		raw, err := run.RunRaw(ctx, diffArgs("--patch")...)
		if err != nil {
			return diffErr(from, to, err)
		}
		sections = splitPatches(raw)
		if len(sections) != len(stats) {
			return fmt.Errorf("diff %s..%s: %d patch sections for %d numstat entries",
				shortSHA(from), shortSHA(to), len(sections), len(stats))
		}
	}

	budget := diffByteBudget
	for i, entry := range stats {
		generated := isGenerated(entry.path)
		file := FileDiff{
			Path:      entry.path,
			StatLine:  entry.statLine,
			Binary:    entry.binary,
			Generated: generated,
		}
		if !patch {
			d.Files = append(d.Files, file)
			continue
		}
		switch {
		case entry.binary:
			d.OmittedStats = append(d.OmittedStats, entry.statLine)
			continue
		case generated && !includeGenerated:
			d.OmittedStats = append(d.OmittedStats, entry.statLine+" (generated; pass include_generated: true for the patch)")
			continue
		}
		section := sections[i]
		if len(section) > budget {
			d.OmittedStats = append(d.OmittedStats, entry.statLine+" (patch exceeds the output budget; use a path filter with a smaller range or read the file at each rev)")
			continue
		}
		budget -= len(section)
		file.Patch = section
		d.Files = append(d.Files, file)
	}
	return nil
}

// diffErr wraps a git diff failure, naming the narrowing options when the
// deadline expired.
func diffErr(from, to string, err error) error {
	if errors.Is(err, gitcmd.ErrTimeout) {
		return fmt.Errorf("diff %s..%s timed out; narrow with a path filter or closer revs: %w", shortSHA(from), shortSHA(to), err)
	}
	return fmt.Errorf("diffing %s..%s: %w", shortSHA(from), shortSHA(to), err)
}

// splitPatches splits raw `git diff --patch` output into whole per-file
// sections in git's emission order, preserving every byte so each section
// applies cleanly on its own. Sections are matched to numstat entries by
// position rather than by path: both invocations use identical diff flags,
// so the file order is identical, and header paths are unreliable —
// core.quotepath C-quotes non-ASCII names in "diff --git" headers while
// --numstat -z emits them raw. Order matching also keeps typechange
// entries distinct, where two sections can share one path.
func splitPatches(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, "\ndiff --git ")
	for i, part := range parts {
		if i > 0 {
			part = "diff --git " + part
		}
		if i < len(parts)-1 {
			// Re-add the newline the split consumed.
			part += "\n"
		}
		parts[i] = part
	}
	return parts
}

// isGenerated reports whether the path's basename matches one of the
// generated/lockfile globs.
func isGenerated(p string) bool {
	base := path.Base(p)
	for _, pattern := range generatedPatterns {
		if ok, _ := path.Match(pattern, base); ok {
			return true
		}
	}
	return false
}
