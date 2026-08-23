package cli

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/broderick-westrope/muninn/internal/status"
	"github.com/broderick-westrope/muninn/internal/xdg"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print last sync result and per-repo failures",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			st, err := status.Read(xdg.StatusPath())
			switch {
			case errors.Is(err, status.ErrNotExist):
				fmt.Fprintln(out, "never synced — run `muninn sync`")
			case err != nil:
				return err
			default:
				printStatus(out, st)
			}
			return nil
		},
	}
}

func printStatus(out io.Writer, st *status.SyncStatus) {
	if st.FinishedAt.IsZero() {
		fmt.Fprintf(out, "last sync: started %s, not finished (in progress or interrupted)\n", st.StartedAt.Format(time.RFC1123))
	} else {
		fmt.Fprintf(out, "last sync: %s (%s ago)\n", st.FinishedAt.Format(time.RFC1123), status.Age(st).Truncate(time.Second))
	}

	failed := failedRepos(st)
	switch {
	case st.Success:
		fmt.Fprintf(out, "result: success (%d repos)\n", len(st.Repos))
	case len(failed) == 0:
		fmt.Fprintf(out, "result: failed (run aborted; %d repos retained from previous run)\n", len(st.Repos))
	default:
		fmt.Fprintf(out, "result: failed (%d of %d repos)\n", len(failed), len(st.Repos))
	}

	if len(failed) > 0 {
		w := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
		fmt.Fprintln(w, "REPO\tERROR")
		for _, name := range failed {
			fmt.Fprintf(w, "%s\t%s\n", name, st.Repos[name].Error)
		}
		w.Flush()
	}
}

// failedRepos returns the names of repos with errors, sorted.
func failedRepos(st *status.SyncStatus) []string {
	var failed []string
	for name, rs := range st.Repos {
		if rs.Error != "" {
			failed = append(failed, name)
		}
	}
	sort.Strings(failed)
	return failed
}
