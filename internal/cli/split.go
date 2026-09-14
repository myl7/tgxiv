package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/myl7/tgxiv/internal/split"
)

// newSplitCmd moves dialogs out of the archive root the shared --dir flag
// points at into the root given by --to. It reads the config via baseConfig
// directly instead of openArchive: no tdl runner is involved, and an Archive
// would set up the export dir and runner this move never touches.
func newSplitCmd() *cobra.Command {
	var to string

	cmd := &cobra.Command{
		Use:   "split --to DIR <dialog_id>[,<dialog_id>...]",
		Short: "Move dialogs out of this archive root into another archive root (rows, media, and watermarks)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := baseConfig()
			if err != nil {
				return err
			}
			if to == "" {
				return fmt.Errorf("--to is required (the destination archive root, created if absent)")
			}
			if to == cfg.Dir {
				return fmt.Errorf("--to must be a different archive root than -d (both are %s)", to)
			}
			ids, err := split.ParseIDs(args)
			if err != nil {
				return err
			}
			return split.Run(cfg.Dir, to, ids)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "destination archive root (created if absent; must differ from -d)")
	return cmd
}
