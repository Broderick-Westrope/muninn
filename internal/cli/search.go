package cli

import "github.com/spf13/cobra"

func newSearchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search [pattern]",
		Short: "Search the index from the terminal",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errNotImplemented
		},
	}
}
