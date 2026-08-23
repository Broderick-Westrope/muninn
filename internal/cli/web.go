package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newWebCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "web",
		Short: "Start the local web UI for interactive search",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("not implemented")
		},
	}
}
