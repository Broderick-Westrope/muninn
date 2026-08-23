package cli

import "github.com/spf13/cobra"

func newSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Reconcile repos: clone/fetch mirrors, build index, GC removed repos",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errNotImplemented
		},
	}
}
