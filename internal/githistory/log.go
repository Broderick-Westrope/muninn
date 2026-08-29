package githistory

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/broderick-westrope/muninn/internal/gitcmd"
	"github.com/broderick-westrope/muninn/internal/gitfile"
)

// defaultLogLimit is used when LogOptions.Limit is not positive.
const defaultLogLimit = 30

// LogOptions are the filters for SearchCommits. All fields are optional
// except that ChangedLiteral and ChangedRegex are mutually exclusive.
type LogOptions struct {
	// Rev is the starting revision; it must resolve in the mirror.
	Rev string
	// Author filters by author (git --author regex).
	Author string
	// Since and Until bound the commit date (git --since/--until); note
	// output dates are author dates.
	Since string
	// Until bounds the commit date from above.
	Until string
	// Path restricts the walk to commits touching the literal path, with
	// --follow so history continues past renames.
	Path string
	// Message filters by commit message (git --grep).
	Message string
	// ChangedLiteral is the pickaxe -S filter: commits where the
	// occurrence count of the literal string changed.
	ChangedLiteral string
	// ChangedRegex is the pickaxe -G filter: commits whose diff has a
	// hunk matching the regex.
	ChangedRegex string
	// FirstParent controls --first-parent; nil means true.
	FirstParent *bool
	// Limit caps the number of returned commits; <= 0 means 30.
	Limit int
}

// SearchCommits runs git log over the bare mirror at mirrorDir with the
// given filters. On timeout it returns the commits parsed from the partial
// output with timedOut set, so agents get labeled partial results instead
// of a hang. Truncated reports that more commits matched than Limit.
func SearchCommits(ctx context.Context, mirrorDir string, opts LogOptions) (commits []Commit, truncated, timedOut bool, err error) {
	if opts.ChangedLiteral != "" && opts.ChangedRegex != "" {
		return nil, false, false, errors.New("changed_literal and changed_regex are mutually exclusive")
	}
	if err := validatePath(opts.Path); err != nil {
		return nil, false, false, err
	}
	sha, err := gitfile.ResolveRev(ctx, mirrorDir, opts.Rev)
	if err != nil {
		return nil, false, false, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultLogLimit
	}

	// %P is empty for root commits and space-separated for merges; %s may
	// contain tabs, so it is the last field and parsing uses SplitN.
	args := []string{
		"-C", mirrorDir, "log",
		"--format=%H%x09%as%x09%an%x09%P%x09%s",
		"-n", strconv.Itoa(limit + 1),
	}
	if opts.FirstParent == nil || *opts.FirstParent {
		args = append(args, "--first-parent")
	}
	if opts.Author != "" {
		args = append(args, "--author="+opts.Author)
	}
	if opts.Since != "" {
		args = append(args, "--since="+opts.Since)
	}
	if opts.Until != "" {
		args = append(args, "--until="+opts.Until)
	}
	if opts.Message != "" {
		args = append(args, "--grep="+opts.Message)
	}
	if opts.ChangedLiteral != "" {
		args = append(args, "-S"+opts.ChangedLiteral)
	}
	if opts.ChangedRegex != "" {
		args = append(args, "-G"+opts.ChangedRegex)
	}
	if opts.Path != "" {
		args = append(args, "--follow")
	}
	args = append(args, "--end-of-options", sha)
	if opts.Path != "" {
		args = append(args, "--", opts.Path)
	}

	out, err := runner(logTimeout).RunRaw(ctx, args...)
	if err != nil {
		if !errors.Is(err, gitcmd.ErrTimeout) {
			return nil, false, false, fmt.Errorf("searching commits from %s: %w", shortSHA(sha), err)
		}
		// Partial stdout is returned on error; keep the complete lines.
		timedOut = true
	}

	lines := strings.Split(out, "\n")
	if len(lines) > 0 && (timedOut || lines[len(lines)-1] == "") {
		// Drop the trailing empty element on success, or the possibly
		// incomplete final fragment on timeout.
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		if line == "" {
			continue
		}
		commit, parseErr := parseLogLine(line)
		if parseErr != nil {
			if timedOut {
				// A killed git may leave a garbled tail; skip it.
				continue
			}
			return nil, false, false, parseErr
		}
		commits = append(commits, commit)
	}
	if len(commits) > limit {
		commits = commits[:limit]
		truncated = true
	}
	return commits, truncated, timedOut, nil
}

// parseLogLine parses one %H%x09%as%x09%an%x09%P%x09%s log line. The
// subject is the last field so SplitN preserves any tabs it contains; the
// parents field is empty for root commits.
func parseLogLine(line string) (Commit, error) {
	fields := strings.SplitN(line, "\t", 5)
	if len(fields) != 5 {
		return Commit{}, fmt.Errorf("malformed log line %q", line)
	}
	return Commit{
		SHA:        fields[0],
		AuthorDate: fields[1],
		Author:     fields[2],
		IsMerge:    len(strings.Fields(fields[3])) > 1,
		Subject:    fields[4],
	}, nil
}
