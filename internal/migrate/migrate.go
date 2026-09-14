// Package migrate is the legacy-import module. Two entry points, both
// deliberately offline (no tdl call): `migrate [FILE...]` backfills archive
// content from tdl export JSON that predates the DB-stores-content design —
// the stamped snapshots the old pipeline kept in export/, or any "tdl chat
// export" file handed as an argument — and `migrate db` converts one old
// one-channel-per-directory archive (v2 archive.db plus flat media/ files)
// into a dialog of a current archive root, run once per old directory.
// Because the package only serves the transition, it can be deleted once
// every old archive is converted, along with its single wiring line in
// internal/cli/root.go and the README rows. Archive.Import stays in
// internal/archive because the live Export path uses it.
package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/spf13/cobra"

	"github.com/myl7/tgxiv/internal/archive"
	"github.com/myl7/tgxiv/internal/store"
)

// stampExportRe matches the file names of the stamped export snapshots the
// JSON-era pipeline kept in export/ (tdl's 20060102-150405 naming). Anything
// else in that dir (batch.json, stray files) is transient and skipped.
var stampExportRe = regexp.MustCompile(`^\d{8}-\d{6}\.json$`)

// replay backfills the messages content table of an old archive from the
// stamped export JSON snapshots still kept in <dir>/export/. The files are
// replayed oldest first (for the stamp naming, lexical order is chronological
// order), so the newest snapshot of a message wins — the same merge rule the
// old viewer used. Each snapshot goes through Import: content rows are fully
// rewritten, the manifest is refreshed without touching download progress, the
// watermark advances monotonically, and the snapshot files themselves are never
// deleted. The whole operation is offline (no tdl call). A missing directory or
// no matching snapshots is not an error; the caller decides the message.
func replay(a *archive.Archive, exportDir string) (files int, err error) {
	entries, err := os.ReadDir(exportDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() || !stampExportRe.MatchString(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names) // oldest first; os.ReadDir already sorts, this pins it

	for _, name := range names {
		if _, err := a.Import(filepath.Join(exportDir, name)); err != nil {
			return files, fmt.Errorf("replay %s: %w", name, err)
		}
		files++
	}
	return files, nil
}

// New builds the migrate command, which is offline: it never invokes tdl. Its
// default form replays export JSON; the `db` subcommand converts an old v2
// channel archive directory.
func New(cfg func() (archive.Config, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate [FILE...]",
		Short: "Backfill archive content from old export JSON, or convert an old v2 channel archive dir with `migrate db` (offline, no tdl call)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cfg()
			if err != nil {
				return err
			}
			a, err := archive.Open(c)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			if len(args) == 0 {
				files, err := replay(a, filepath.Join(c.Dir, "export"))
				if err != nil {
					return err
				}
				if files == 0 {
					fmt.Println("[archive] no export snapshots to replay (this is fine for new archives)")
				} else {
					fmt.Printf("[archive] replayed %d export snapshot(s)\n", files)
				}
			}
			for _, f := range args {
				added, err := a.Import(f)
				if err != nil {
					return err
				}
				fmt.Printf("imported %s; %d new media messages added to manifest\n", f, added)
			}

			// content rolls up across every dialog the replay touched
			dialogs, err := a.Store().ListDialogs()
			if err != nil {
				return err
			}
			msgs := 0
			for _, d := range dialogs {
				n, err := a.Store().MessageCount(d.DialogID)
				if err != nil {
					return err
				}
				msgs += n
			}
			counts, err := a.Store().CountsAll()
			if err != nil {
				return err
			}
			fmt.Printf("content: %d message(s) across %d dialog(s)\n", msgs, len(dialogs))
			fmt.Printf("total:   %d\n", counts["total"])
			fmt.Printf("done:    %d\n", counts[store.StatusDone])
			fmt.Printf("pending: %d\n", counts[store.StatusPending])
			fmt.Printf("failed:  %d\n", counts[store.StatusFailed])
			return nil
		},
	}
	cmd.AddCommand(newDBCmd(cfg))
	cmd.AddCommand(newSetDialogCmd(cfg))
	return cmd
}
