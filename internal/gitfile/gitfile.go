// Package gitfile reads file contents and directory listings from bare git
// mirrors, pinned to a specific commit (the indexed commit recorded in the
// status file) so line numbers always agree with search results.
package gitfile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
)

// maxBlobBytes is the largest blob ReadFile will return.
const maxBlobBytes = 10 << 20 // 10 MiB

// ErrIndexMismatch is returned when the requested commit is not reachable in
// the mirror: the index and the mirror are out of sync (for example after a
// force-push followed by a failed index run). Running `muninn sync` repairs
// it.
var ErrIndexMismatch = errors.New("indexed commit not found in mirror; index and mirror are out of sync, run `muninn sync`")

// ErrFileTooLarge is returned by ReadFile for blobs larger than 10 MiB.
var ErrFileTooLarge = errors.New("file too large to read (over 10 MiB)")

// TreeEntry is one entry of a directory listing.
type TreeEntry struct {
	// Path is the entry path relative to the repo root.
	Path string
	// Type is "file" or "dir".
	Type string
	// Size is the blob size in bytes; 0 for directories.
	Size int64
}

// ReadFile returns the contents of path at commit in the bare mirror at
// mirrorDir, plus the file's total line count.
//
// Offset and limit are line-based and 1-indexed: offset is the first line to
// return (0 is treated as 1) and limit is the number of lines. A limit of 0
// returns the whole file; a negative offset or limit is an error. Blobs over
// 10 MiB yield ErrFileTooLarge. An unreachable commit yields
// ErrIndexMismatch; a missing path yields an error wrapping fs.ErrNotExist.
func ReadFile(ctx context.Context, mirrorDir, commit, path string, offset, limit int) (content string, totalLines int, err error) {
	if offset < 0 {
		return "", 0, fmt.Errorf("offset %d is negative", offset)
	}
	if limit < 0 {
		return "", 0, fmt.Errorf("limit %d is negative", limit)
	}
	if offset == 0 {
		offset = 1
	}
	if err := checkCommit(ctx, mirrorDir, commit); err != nil {
		return "", 0, err
	}

	obj := commit + ":" + path
	objType, err := runGit(ctx, "-C", mirrorDir, "cat-file", "-t", obj)
	if err != nil {
		return "", 0, fmt.Errorf("path %q not found at commit %s: %w", path, shortSHA(commit), fs.ErrNotExist)
	}
	if objType != "blob" {
		return "", 0, fmt.Errorf("path %q at commit %s is a %s, not a file", path, shortSHA(commit), objType)
	}

	sizeStr, err := runGit(ctx, "-C", mirrorDir, "cat-file", "-s", obj)
	if err != nil {
		return "", 0, fmt.Errorf("sizing %q at commit %s: %w", path, shortSHA(commit), err)
	}
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("parsing size %q of %q: %w", sizeStr, path, err)
	}
	if size > maxBlobBytes {
		return "", 0, fmt.Errorf("%q at commit %s is %d bytes: %w", path, shortSHA(commit), size, ErrFileTooLarge)
	}

	// Blob content must be returned byte-exact, so skip runGit's trimming.
	blob, err := runGitRaw(ctx, "-C", mirrorDir, "cat-file", "blob", obj)
	if err != nil {
		return "", 0, fmt.Errorf("reading %q at commit %s: %w", path, shortSHA(commit), err)
	}
	return window(blob, offset, limit)
}

// window slices blob to the requested 1-indexed line range, preserving the
// original line terminators. A limit of 0 means all lines from offset on.
func window(blob string, offset, limit int) (content string, totalLines int, err error) {
	if blob == "" {
		return "", 0, nil
	}
	// Lines are newline-terminated; a file not ending in a newline still
	// counts its final fragment as a line.
	lines := strings.SplitAfter(blob, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	totalLines = len(lines)

	if offset > totalLines {
		return "", totalLines, nil
	}
	end := totalLines
	if limit > 0 && offset-1+limit < end {
		end = offset - 1 + limit
	}
	return strings.Join(lines[offset-1:end], ""), totalLines, nil
}

// ListTree lists the entries under path at commit in the bare mirror at
// mirrorDir, descending depth levels below path (depth < 1 is treated as 1).
// Path "" lists from the repo root. An unreachable commit yields
// ErrIndexMismatch; a missing path yields an error wrapping fs.ErrNotExist.
func ListTree(ctx context.Context, mirrorDir, commit, path string, depth int) ([]TreeEntry, error) {
	if depth < 1 {
		depth = 1
	}
	if err := checkCommit(ctx, mirrorDir, commit); err != nil {
		return nil, err
	}

	// -r -t lists blobs and trees recursively in one call; entries deeper
	// than the requested depth are filtered out below. -z avoids quoting of
	// unusual path names, --long adds blob sizes.
	args := []string{"-C", mirrorDir, "ls-tree", "-r", "-t", "--long", "-z", commit}
	prefix := strings.Trim(path, "/")
	if prefix != "" {
		args = append(args, "--", prefix)
	}
	out, err := runGit(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("listing tree %q at commit %s: %w", path, shortSHA(commit), err)
	}

	var entries []TreeEntry
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		entry, err := parseTreeEntry(record)
		if err != nil {
			return nil, fmt.Errorf("listing tree %q at commit %s: %w", path, shortSHA(commit), err)
		}
		if relativeDepth(entry.Path, prefix) > depth {
			continue
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		// ls-tree exits 0 with empty output for a pathspec that matches
		// nothing; distinguish that from a genuinely empty tree at root.
		if prefix != "" {
			return nil, fmt.Errorf("path %q not found at commit %s: %w", path, shortSHA(commit), fs.ErrNotExist)
		}
	}
	return entries, nil
}

// parseTreeEntry parses one `ls-tree --long` record:
// "<mode> <type> <sha> <size>\t<path>".
func parseTreeEntry(record string) (TreeEntry, error) {
	meta, name, ok := strings.Cut(record, "\t")
	if !ok {
		return TreeEntry{}, fmt.Errorf("malformed ls-tree record %q", record)
	}
	fields := strings.Fields(meta)
	if len(fields) != 4 {
		return TreeEntry{}, fmt.Errorf("malformed ls-tree record %q", record)
	}
	entry := TreeEntry{Path: name}
	switch fields[1] {
	case "tree":
		entry.Type = "dir"
	case "blob":
		entry.Type = "file"
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return TreeEntry{}, fmt.Errorf("parsing size in ls-tree record %q: %w", record, err)
		}
		entry.Size = size
	default:
		// Submodules (commit entries) and other exotica are skipped by
		// reporting them as files with no size.
		entry.Type = "file"
	}
	return entry, nil
}

// relativeDepth returns how many levels below prefix the path sits
// (1 for a direct child).
func relativeDepth(path, prefix string) int {
	rel := path
	if prefix != "" {
		rel = strings.TrimPrefix(path, prefix+"/")
	}
	return strings.Count(rel, "/") + 1
}

// checkCommit verifies the commit exists and is reachable in the mirror,
// returning ErrIndexMismatch otherwise.
func checkCommit(ctx context.Context, mirrorDir, commit string) error {
	if _, err := runGit(ctx, "-C", mirrorDir, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return fmt.Errorf("commit %s: %w", shortSHA(commit), ErrIndexMismatch)
	}
	return nil
}

// shortSHA abbreviates a commit SHA for error messages.
func shortSHA(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// runGit executes a git command and returns its trimmed stdout.
func runGit(ctx context.Context, args ...string) (string, error) {
	out, err := runGitRaw(ctx, args...)
	return strings.TrimSpace(out), err
}

// runGitRaw executes a git command and returns its stdout verbatim.
func runGitRaw(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
