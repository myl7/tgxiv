package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/myl7/tgxiv/internal/archive"
	"github.com/myl7/tgxiv/internal/store"
)

// setDialogOptions carries the `migrate set-dialog` flags.
type setDialogOptions struct {
	Username string
	Title    string
	Kind     string
}

// newSetDialogCmd builds the `migrate set-dialog` subcommand: set or fix one
// dialog's title/username/kind by hand, directly in the root's database. It
// exists because the automatic metadata refresh (tdl chat ls on every export)
// needs the dialog to still exist on Telegram — a deleted channel can never
// be named that way — and because `migrate db`'s --title/--username/--kind
// only work while the old directory, the conversion's data source, still
// exists. It rides the migrate module so it dies with it once every old
// archive is converted. Like the rest of this package it is offline: no tdl
// call, no archive.Open.
func newSetDialogCmd(cfg func() (archive.Config, error)) *cobra.Command {
	var opts setDialogOptions
	cmd := &cobra.Command{
		Use:   "set-dialog <dialog_id>",
		Short: "Set or fix a dialog's title/username/kind by hand — the remedy for channels deleted on Telegram (offline, no tdl call)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Title == "" && opts.Username == "" && opts.Kind == "" {
				return fmt.Errorf("nothing to set: pass at least one of --title, --username, --kind")
			}
			// a bare positive id: the marked -100… form cannot reach a cobra
			// POSITIONAL argument (a leading dash parses as a flag), so only
			// the stored form is accepted
			dialogID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || dialogID <= 0 {
				return fmt.Errorf("dialog id %q must be a bare positive integer (the stored form; see `tgxiv status`)", args[0])
			}
			c, err := cfg()
			if err != nil {
				return err
			}
			stored, err := setDialogMeta(c, dialogID, store.Dialog{
				Username: strings.TrimPrefix(opts.Username, "@"), // stored bare, no '@'
				Title:    opts.Title,
				Kind:     opts.Kind,
			})
			if err != nil {
				return err
			}
			printDialogRow(stored)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.Username, "username", "", "dialog username, stored without a leading '@' (optional)")
	f.StringVar(&opts.Title, "title", "", "dialog title (optional)")
	f.StringVar(&opts.Kind, "kind", "", "dialog kind: channel | group | private (stored leniently; optional)")
	return cmd
}

// setDialogMeta merges non-empty metadata fields into an existing dialog row
// of the root's tgxiv.sqlite, reads the row back, and refreshes its
// media-folder marker — the manual remedy for dialogs whose metadata can never
// be auto-refreshed (channels deleted on Telegram). d's zero fields mean
// "leave unchanged", which is exactly UpsertDialog's non-clobber merge;
// last_msg_id is never touched, so a metadata edit can never rewind the sync
// watermark. Offline like all of migrate: the root's DB is opened directly, no
// archive.Open, no tdl. The marker write is best-effort: a failure warns on
// stderr and the stored row is still returned.
func setDialogMeta(c archive.Config, dialogID int64, d store.Dialog) (store.Dialog, error) {
	// opened by path like migrateDB does: archive.Open's side effects (tdl
	// runner setup, root scaffolding) are exactly what an offline edit avoids
	st, err := store.Open(filepath.Join(c.Dir, "tgxiv.sqlite"))
	if err != nil {
		return store.Dialog{}, err
	}
	defer func() { _ = st.Close() }()

	// UpsertDialog INSERTS a missing row, so guard first: a typo'd id must be
	// an actionable error, not a freshly minted phantom dialog
	if _, ok, err := st.GetDialog(dialogID); err != nil {
		return store.Dialog{}, err
	} else if !ok {
		return store.Dialog{}, fmt.Errorf("dialog %d is not in this archive; run `tgxiv status` to list dialogs", dialogID)
	}
	if err := st.UpsertDialog(store.Dialog{
		DialogID: dialogID,
		Username: d.Username,
		Title:    d.Title,
		Kind:     d.Kind,
	}); err != nil {
		return store.Dialog{}, err
	}
	// the merge result — not this call's possibly-empty fields — decides both
	// the returned row and what the marker says
	stored, ok, err := st.GetDialog(dialogID)
	if err != nil {
		return store.Dialog{}, err
	}
	if !ok {
		return store.Dialog{}, fmt.Errorf("dialog %d missing right after upsert; state is inconsistent", dialogID)
	}
	if err := archive.WriteDialogMarker(c.Dir, stored); err != nil {
		fmt.Fprintf(os.Stderr, "[migrate] dialog marker: %v\n", err)
	}
	return stored, nil
}

// dialogLabel mirrors cli's unexported dialogLabel (internal/cli cannot be
// imported from here without a cycle in the wiring): the bare id when nothing
// is known, else "id (title @username)" — so this command's output greps
// alongside `tgxiv status`.
func dialogLabel(d store.Dialog) string {
	var parts []string
	if d.Title != "" {
		parts = append(parts, d.Title)
	}
	if d.Username != "" {
		parts = append(parts, "@"+d.Username)
	}
	if len(parts) == 0 {
		return strconv.FormatInt(d.DialogID, 10)
	}
	return fmt.Sprintf("%d (%s)", d.DialogID, strings.Join(parts, " "))
}

// printDialogRow prints the stored row in the status block's label style —
// `dialog <label>` then one line per set metadata field — plain and
// grep-friendly like the rest of the CLI output.
func printDialogRow(d store.Dialog) {
	fmt.Printf("dialog %s\n", dialogLabel(d))
	if d.Title != "" {
		fmt.Printf("  title: %s\n", d.Title)
	}
	if d.Username != "" {
		fmt.Printf("  username: %s\n", d.Username)
	}
	if d.Kind != "" {
		fmt.Printf("  kind: %s\n", d.Kind)
	}
}
