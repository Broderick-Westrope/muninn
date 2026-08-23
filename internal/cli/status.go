package cli

import "github.com/spf13/cobra"

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print last sync result and per-repo failures",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errNotImplemented
		},
	}
}
