package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myl7/tgxiv/internal/archive"
	"github.com/myl7/tgxiv/internal/store"
)

// seedDialogRow opens a v3 root's tgxiv.sqlite directly — the same way
// setDialogMeta does — and plants one dialog row, returning the open store
// for assertions.
func seedDialogRow(t *testing.T, dir string, d store.Dialog) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "tgxiv.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpsertDialog(d); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestSetDialogMetaMergesFields pins the manual-metadata remedy: each non-empty
// field merges into the stored row while the fields not passed keep their
// values, the returned row and media/<id>/dialog.txt both reflect the merge,
// and the sync watermark survives — a metadata edit must never rewind it.
func TestSetDialogMetaMergesFields(t *testing.T) {
	const wm = 2913
	seeded := store.Dialog{DialogID: 100, Username: "somechannel", Title: "Old Title", Kind: "private"}
	for _, tc := range []struct {
		name   string
		set    store.Dialog
		want   store.Dialog
		marker string
	}{
		{
			name:   "title",
			set:    store.Dialog{Title: "Renamed"},
			want:   store.Dialog{DialogID: 100, Username: "somechannel", Title: "Renamed", Kind: "private"},
			marker: "dialog_id: 100\ntitle: Renamed\nusername: somechannel\n",
		},
		{
			name:   "username",
			set:    store.Dialog{Username: "newname"},
			want:   store.Dialog{DialogID: 100, Username: "newname", Title: "Old Title", Kind: "private"},
			marker: "dialog_id: 100\ntitle: Old Title\nusername: newname\n",
		},
		{
			// kind is DB-only metadata: the marker carries no kind line
			name:   "kind",
			set:    store.Dialog{Kind: "group"},
			want:   store.Dialog{DialogID: 100, Username: "somechannel", Title: "Old Title", Kind: "group"},
			marker: "dialog_id: 100\ntitle: Old Title\nusername: somechannel\n",
		},
		{
			// the func does not demand a field (the command does): all-empty is
			// the merge contract's no-op and must never wipe stored values
			name:   "all fields empty never wipes",
			set:    store.Dialog{},
			want:   seeded,
			marker: "dialog_id: 100\ntitle: Old Title\nusername: somechannel\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st := seedDialogRow(t, dir, seeded)
			if err := st.AdvanceLastMsgID(100, wm); err != nil {
				t.Fatal(err)
			}

			d, err := setDialogMeta(archive.Config{Dir: dir}, 100, tc.set)
			if err != nil {
				t.Fatal(err)
			}
			if d.DialogID != tc.want.DialogID || d.Username != tc.want.Username || d.Title != tc.want.Title || d.Kind != tc.want.Kind {
				t.Errorf("returned dialog = %+v, want %+v", d, tc.want)
			}
			if d.LastMsgID != wm {
				t.Errorf("returned LastMsgID = %d, want %d", d.LastMsgID, wm)
			}
			// the DB agrees, watermark untouched
			stored, ok, err := st.GetDialog(100)
			if err != nil || !ok {
				t.Fatalf("GetDialog = %v,%v; want the dialog", ok, err)
			}
			if stored.Username != tc.want.Username || stored.Title != tc.want.Title || stored.Kind != tc.want.Kind || stored.LastMsgID != wm {
				t.Errorf("stored dialog = %+v, want %+v with watermark %d", stored, tc.want, wm)
			}
			// the marker reflects the merged row, not the call's empties
			b, err := os.ReadFile(filepath.Join(dir, "media", "100", "dialog.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.marker {
				t.Errorf("dialog.txt = %q, want %q", b, tc.marker)
			}
		})
	}
}

// TestSetDialogMetaUnknownDialogErrors: an unknown id is an actionable error
// naming the status remedy, never a silently minted dialog row.
func TestSetDialogMetaUnknownDialogErrors(t *testing.T) {
	dir := t.TempDir()
	st := seedDialogRow(t, dir, store.Dialog{DialogID: 100})

	_, err := setDialogMeta(archive.Config{Dir: dir}, 999, store.Dialog{Title: "Ghost"})
	if err == nil || !strings.Contains(err.Error(), "999") || !strings.Contains(err.Error(), "tgxiv status") {
		t.Fatalf("err = %v, want it naming 999 and the status remedy", err)
	}
	if _, ok, err := st.GetDialog(999); err != nil || ok {
		t.Errorf("GetDialog(999) = %v,%v; want no dialog minted", ok, err)
	}
}

// runSetDialog executes `migrate set-dialog` with args against a fixed cfg,
// returning the command error.
func runSetDialog(t *testing.T, c archive.Config, args ...string) error {
	t.Helper()
	cmd := newSetDialogCmd(func() (archive.Config, error) { return c, nil })
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(args)
	return cmd.Execute()
}

// TestSetDialogCommand drives the cobra path end to end: --username loses its
// leading '@', and the stored row plus marker end up merged with the watermark
// untouched. The command's own rejections — no flag, non-numeric id, unknown
// id — are checked here too, since they live above setDialogMeta.
func TestSetDialogCommand(t *testing.T) {
	dir := t.TempDir()
	st := seedDialogRow(t, dir, store.Dialog{DialogID: 100, Title: "Old Title"})
	if err := st.AdvanceLastMsgID(100, 2913); err != nil {
		t.Fatal(err)
	}

	// the stored bare id: the marked -100… form is unusable as a positional
	// argument (cobra parses its leading dash as a flag)
	if err := runSetDialog(t, archive.Config{Dir: dir}, "100",
		"--username", "@newname", "--title", "New Title", "--kind", "channel"); err != nil {
		t.Fatal(err)
	}

	d, ok, err := st.GetDialog(100)
	if err != nil || !ok {
		t.Fatalf("GetDialog = %v,%v; want the dialog", ok, err)
	}
	if d.Username != "newname" || d.Title != "New Title" || d.Kind != "channel" {
		t.Errorf("dialog = %+v, want the '@'-stripped username plus the new title/kind", d)
	}
	if d.LastMsgID != 2913 {
		t.Errorf("LastMsgID = %d, want 2913 untouched", d.LastMsgID)
	}
	b, err := os.ReadFile(filepath.Join(dir, "media", "100", "dialog.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "dialog_id: 100\ntitle: New Title\nusername: newname\n"; string(b) != want {
		t.Errorf("dialog.txt = %q, want %q", b, want)
	}

	// at least one flag is required, and the error names all three
	if err := runSetDialog(t, archive.Config{Dir: dir}, "100"); err == nil ||
		!strings.Contains(err.Error(), "nothing to set") ||
		!strings.Contains(err.Error(), "--title") || !strings.Contains(err.Error(), "--username") || !strings.Contains(err.Error(), "--kind") {
		t.Fatalf("no flags: err = %v, want \"nothing to set\" naming the three flags", err)
	}
	// a non-numeric id is actionable, not a zero dialog
	if err := runSetDialog(t, archive.Config{Dir: dir}, "notachannel", "--title", "x"); err == nil ||
		!strings.Contains(err.Error(), "notachannel") {
		t.Fatalf("non-numeric id: err = %v, want it naming notachannel", err)
	}
	// an unknown id stays the actionable error through the command path
	if err := runSetDialog(t, archive.Config{Dir: dir}, "999", "--title", "x"); err == nil ||
		!strings.Contains(err.Error(), "tgxiv status") {
		t.Fatalf("unknown id: err = %v, want it pointing at tgxiv status", err)
	}
}
