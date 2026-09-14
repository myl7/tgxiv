// Package migrate is the legacy-import module: it backfills archive.db from
// tdl export JSON that predates the DB-stores-content design. That means the
// stamped snapshots the old pipeline kept in <dir>/export/, or any
// "tdl chat export" file handed as an argument. It is deliberately
// self-contained so that once every old archive is migrated, the whole package
// can be deleted along with its single wiring line in internal/cli/root.go and
// the README rows. Archive.Import stays in internal/archive because the live
// Export path uses it.
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

// New builds the migrate command, which is offline: it never invokes tdl.
func New(cfg func() (archive.Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate [FILE...]",
		Short: "Backfill archive.db from old export JSON: JSON-era snapshots in export/, or files given as args (offline, no tdl call)",
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

			msgs, err := a.Store().MessageCount()
			if err != nil {
				return err
			}
			counts, err := a.Store().Counts()
			if err != nil {
				return err
			}
			fmt.Printf("content: %d message(s)\n", msgs)
			fmt.Printf("total:   %d\n", counts["total"])
			fmt.Printf("done:    %d\n", counts[store.StatusDone])
			fmt.Printf("pending: %d\n", counts[store.StatusPending])
			fmt.Printf("failed:  %d\n", counts[store.StatusFailed])
			return nil
		},
	}
}
