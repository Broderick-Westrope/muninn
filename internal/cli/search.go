package cli

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/broderick-westrope/muninn/internal/search"
	"github.com/broderick-westrope/muninn/internal/xdg"
)

// ANSI escape codes used to color the repo/path:line prefix on a TTY.
const (
	ansiReset   = "\x1b[0m"
	ansiMagenta = "\x1b[35m"
	ansiGreen   = "\x1b[32m"
)

func newSearchCmd() *cobra.Command {
	var (
		repoFilter string
		limit      int
		filesOnly  bool
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the index from the terminal",
		Long: `Search the index with a zoekt-syntax query (regex plus atoms like
repo:, file:, sym:, lang:, case:, and -negation; space-separated atoms are ANDed).

Matches print to stdout as 'repo/path:line: content' (colored on a TTY, plain
when piped); stats and truncation notices go to stderr.

Exit codes follow grep: 0 with matches, 1 without, 2 on error.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Zoekt logs shard loading via the stdlib logger; that noise
			// belongs in sync logs, not interactive search output.
			log.SetOutput(io.Discard)
			searcher, err := search.Open(xdg.IndexDir())
			if err != nil {
				return &ExitError{Code: 2, Err: err}
			}
			defer searcher.Close()

			// Fetch one line past the cap so truncation is detected even
			// when zoekt's own display limits would round up.
			res, err := searcher.Search(cmd.Context(), search.Options{
				Query:      args[0],
				RepoFilter: repoFilter,
				MaxResults: limit + 1,
			})
			if err != nil {
				return &ExitError{Code: 2, Err: err}
			}

			color := isTerminal(os.Stdout)
			var out searchOutput
			if filesOnly {
				out = formatFilesOnly(res, limit, color)
			} else {
				out = formatMatches(res, limit, color)
			}
			fmt.Fprint(cmd.OutOrStdout(), out.body)
			printSearchStats(cmd.ErrOrStderr(), res, out, filesOnly)

			if out.shown == 0 {
				return &ExitError{Code: 1}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFilter, "repo", "", "repo name regex to search in")
	cmd.Flags().IntVar(&limit, "limit", 50, "max results to print")
	cmd.Flags().BoolVar(&filesOnly, "files-only", false, "print matching file paths only (deduped)")
	return cmd
}

// searchOutput is a rendered search result ready for printing.
type searchOutput struct {
	// body is the stdout payload, empty when nothing matched.
	body string
	// shown is the number of printed results (line matches, or files
	// when --files-only is set).
	shown int
	// files is the number of distinct files the printed results span.
	files int
	// truncated reports that more results exist than were printed.
	truncated bool
}

// formatMatches renders line matches as 'repo/path:line: content', capped at
// limit lines. With color, the repo/path prefix is magenta and the line
// number green; the content is left uncolored (the search core does not
// expose match offsets within the line).
func formatMatches(res *search.Result, limit int, color bool) searchOutput {
	var b strings.Builder
	out := searchOutput{truncated: res.Truncated}
outer:
	for _, f := range res.Files {
		if out.shown >= limit {
			out.truncated = true
			break
		}
		out.files++
		if len(f.Lines) == 0 {
			// Filename-only match: the path itself is the result.
			fmt.Fprintf(&b, "%s (filename match)\n", colorize(f.Repo+"/"+f.Path, ansiMagenta, color))
			out.shown++
			continue
		}
		for _, l := range f.Lines {
			if out.shown >= limit {
				out.truncated = true
				break outer
			}
			fmt.Fprintf(&b, "%s:%s: %s\n",
				colorize(f.Repo+"/"+f.Path, ansiMagenta, color),
				colorize(fmt.Sprintf("%d", l.LineNumber), ansiGreen, color),
				l.Line)
			out.shown++
		}
	}
	out.body = b.String()
	return out
}

// formatFilesOnly renders distinct matching file paths as 'repo/path',
// deduped and capped at limit files.
func formatFilesOnly(res *search.Result, limit int, color bool) searchOutput {
	var b strings.Builder
	out := searchOutput{truncated: res.Truncated}
	seen := make(map[string]bool)
	for _, f := range res.Files {
		path := f.Repo + "/" + f.Path
		if seen[path] {
			continue
		}
		seen[path] = true
		if out.shown >= limit {
			out.truncated = true
			break
		}
		fmt.Fprintf(&b, "%s\n", colorize(path, ansiMagenta, color))
		out.shown++
	}
	out.files = out.shown
	out.body = b.String()
	return out
}

// printSearchStats writes the stats line and any truncation notice to w
// (stderr), keeping stdout pipe-clean.
func printSearchStats(w io.Writer, res *search.Result, out searchOutput, filesOnly bool) {
	if out.truncated {
		fmt.Fprintln(w, "[truncated: more matches exist; raise --limit or narrow the query]")
	}
	if filesOnly {
		fmt.Fprintf(w, "%d files (%d files considered, %s)\n",
			out.shown, res.Stats.FilesConsidered, res.Stats.Duration.Round(time.Millisecond))
		return
	}
	fmt.Fprintf(w, "%d matches in %d files (%d files considered, %s)\n",
		out.shown, out.files, res.Stats.FilesConsidered, res.Stats.Duration.Round(time.Millisecond))
}

// colorize wraps s in the ANSI code when color is on.
func colorize(s, code string, color bool) string {
	if !color {
		return s
	}
	return code + s + ansiReset
}

// isTerminal reports whether f is a character device (a TTY), using only
// the stdlib: pipes and files are not character devices.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
