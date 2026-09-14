package split

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/myl7/tgxiv/internal/store"
)

// seedRoot creates an archive root holding one dialog per listed id, each
// with the full shape a real archived dialog has: a dialog row with metadata
// and a sync watermark of 42, two content rows, two task rows (one done at a
// recorded path, one pending), and a media folder carrying a file plus the
// dialog.txt marker. Seeding goes through store.Open — the same path the
// destination preparation takes — plus its API, so the fixtures can only be
// rows the real pipeline could have written.
func seedRoot(t *testing.T, root string, ids ...int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(root, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	for _, id := range ids {
		chat := strconv.FormatInt(id, 10)
		if err := st.UpsertDialog(store.Dialog{DialogID: id, Username: "u" + chat, Title: "Dialog " + chat, Kind: "channel"}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertContent(id, []store.ContentRecord{
			{MsgID: 10, Type: "message", Date: 1700000001, Text: "hello", Raw: `{"id":10}`},
			{MsgID: 11, Type: "message", Date: 1700000002, Raw: `{"id":11}`},
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.ImportTasks(id, []store.TaskState{
			{MsgID: 10, FileName: "a.jpg", Size: 3, MediaType: "photo", Status: store.StatusDone, ActualSize: 3, Path: "media/" + chat + "/10_a.jpg"},
			{MsgID: 11, FileName: "b.mp4", Size: 4, MediaType: "video", Status: store.StatusPending},
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.AdvanceLastMsgID(id, 42); err != nil {
			t.Fatal(err)
		}

		dir := filepath.Join(root, mediaDirName, chat)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "10_a.jpg"), []byte("aaa"), 0o644); err != nil {
			t.Fatal(err)
		}
		marker := "dialog_id: " + chat + "\ntitle: Dialog " + chat + "\n"
		if err := os.WriteFile(filepath.Join(dir, "dialog.txt"), []byte(marker), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// openRaw opens the root's database directly for assertions.
func openRaw(t *testing.T, root string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// dialogsOf lists the root's dialog ids ascending.
func dialogsOf(t *testing.T, db *sql.DB) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT dialog_id FROM dialogs ORDER BY dialog_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func queryInt(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func queryString(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRunMovesDialogs(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seedRoot(t, src, 111, 222, 333)

	if err := Run(src, dst, []int64{111, 222}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sdb, ddb := openRaw(t, src), openRaw(t, dst)

	// the destination holds exactly the split dialogs; the source keeps the rest
	if got := dialogsOf(t, ddb); !reflect.DeepEqual(got, []int64{111, 222}) {
		t.Errorf("dst dialogs = %v, want [111 222]", got)
	}
	if got := dialogsOf(t, sdb); !reflect.DeepEqual(got, []int64{333}) {
		t.Errorf("src dialogs = %v, want [333]", got)
	}
	for _, id := range []int64{111, 222} {
		for _, table := range []string{"messages", "tasks"} {
			if n := queryInt(t, ddb, "SELECT COUNT(*) FROM "+table+" WHERE dialog_id = ?", id); n != 2 {
				t.Errorf("dst %s of dialog %d = %d, want 2", table, id, n)
			}
		}
	}
	for _, table := range []string{"messages", "tasks"} {
		if n := queryInt(t, sdb, "SELECT COUNT(*) FROM "+table+" WHERE dialog_id = ?", int64(333)); n != 2 {
			t.Errorf("src %s of kept dialog 333 = %d, want 2", table, n)
		}
	}

	// watermarks ride along verbatim: the incremental-sync cursor must not
	// rewind, or the next sync would re-scan the whole dialog
	if wm := queryInt(t, ddb, `SELECT last_msg_id FROM dialogs WHERE dialog_id = ?`, int64(111)); wm != 42 {
		t.Errorf("dst watermark of dialog 111 = %d, want 42", wm)
	}
	// task paths are root-relative and stay unchanged
	if p := queryString(t, ddb, `SELECT path FROM tasks WHERE dialog_id = ? AND msg_id = 10`, int64(111)); p != "media/111/10_a.jpg" {
		t.Errorf("dst task path = %q, want media/111/10_a.jpg", p)
	}

	// media folders moved whole: file and dialog.txt marker intact at dst, gone from src
	for _, id := range []int64{111, 222} {
		chat := strconv.FormatInt(id, 10)
		if b, err := os.ReadFile(filepath.Join(dst, mediaDirName, chat, "10_a.jpg")); err != nil || string(b) != "aaa" {
			t.Errorf("dst media file of dialog %s = %q, %v; want \"aaa\", nil", chat, b, err)
		}
		wantMarker := "dialog_id: " + chat + "\ntitle: Dialog " + chat + "\n"
		if b, err := os.ReadFile(filepath.Join(dst, mediaDirName, chat, "dialog.txt")); err != nil || string(b) != wantMarker {
			t.Errorf("dst dialog.txt of dialog %s = %q, %v; want the moved marker verbatim", chat, b, err)
		}
		if _, err := os.Stat(filepath.Join(src, mediaDirName, chat)); !os.IsNotExist(err) {
			t.Errorf("src media/%s must be gone, stat err = %v", chat, err)
		}
	}
	// the kept dialog's media stays put
	if _, err := os.Stat(filepath.Join(src, mediaDirName, "333", "10_a.jpg")); err != nil {
		t.Errorf("kept dialog's media must stay at src: %v", err)
	}
}

// TestRunResumeSkipsAlreadySplit re-runs the same split: ids the first run
// finished must be recognized and skipped, without error and without
// duplicating rows at the destination.
func TestRunResumeSkipsAlreadySplit(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seedRoot(t, src, 111, 222)

	if err := Run(src, dst, []int64{111}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := Run(src, dst, []int64{111, 222}); err != nil {
		t.Fatalf("resume Run: %v", err)
	}

	sdb, ddb := openRaw(t, src), openRaw(t, dst)
	if got := dialogsOf(t, ddb); !reflect.DeepEqual(got, []int64{111, 222}) {
		t.Errorf("dst dialogs = %v, want [111 222]", got)
	}
	for _, table := range []string{"messages", "tasks"} {
		if n := queryInt(t, ddb, "SELECT COUNT(*) FROM "+table); n != 4 {
			t.Errorf("dst %s = %d, want 4 (no duplicates)", table, n)
		}
	}
	if got := dialogsOf(t, sdb); len(got) != 0 {
		t.Errorf("src dialogs = %v, want none", got)
	}
}

// TestRunUnknownDialog pins the typo case: an id in neither database is a
// hard error naming the id, raised before any media moves.
func TestRunUnknownDialog(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seedRoot(t, src, 111)

	err := Run(src, dst, []int64{111, 999})
	if err == nil {
		t.Fatal("Run with an unknown id: expected an error")
	}
	if !strings.Contains(err.Error(), "999") {
		t.Errorf("error must name the unknown id 999, got: %v", err)
	}
	// classification fails before the media phase, so the valid id is untouched
	if _, err := os.Stat(filepath.Join(src, mediaDirName, "111")); err != nil {
		t.Errorf("classification must fail before media moves: %v", err)
	}
}

// TestRunDialogInBoth pins the collision case: the same dialog seeded into
// both roots by hand is a hard error naming the id — a state this command's
// atomic transaction can never leave behind.
func TestRunDialogInBoth(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seedRoot(t, src, 111)

	st, err := store.Open(filepath.Join(dst, dbName))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDialog(store.Dialog{DialogID: 111, Title: "collision"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	err = Run(src, dst, []int64{111})
	if err == nil {
		t.Fatal("Run with the dialog in both roots: expected an error")
	}
	if !strings.Contains(err.Error(), "111") || !strings.Contains(err.Error(), "BOTH") {
		t.Errorf("error must name dialog 111 and the BOTH state, got: %v", err)
	}
}

// TestRunMissingSource pins the actionable error for a -d that is not an
// archive root: the user is told to point -d at a root, not handed a raw
// sqlite failure.
func TestRunMissingSource(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	err := Run(src, dst, []int64{111})
	if err == nil {
		t.Fatal("Run with no source database: expected an error")
	}
	if !strings.Contains(err.Error(), dbName) || !strings.Contains(err.Error(), "archive root") {
		t.Errorf("error must point the user at the archive root, got: %v", err)
	}
}

// TestRunSourceNotV3 pins the conversion hint: a source still carrying the v2
// layout (messages, no dialogs) is refused with the `migrate db` remedy
// rather than mangled.
func TestRunSourceNotV3(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(src, dbName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE messages (msg_id INTEGER PRIMARY KEY, raw TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	err = Run(src, dst, []int64{111})
	if err == nil {
		t.Fatal("Run against a v2 source: expected an error")
	}
	if !strings.Contains(err.Error(), "migrate db") {
		t.Errorf("error must point at `tgxiv migrate db`, got: %v", err)
	}
}

// TestRunSameRoot pins the --to == -d rejection, including its
// path-equivalent spellings (a trailing "/." cleans to the same root).
func TestRunSameRoot(t *testing.T) {
	root := t.TempDir()
	seedRoot(t, root, 111)

	if err := Run(root, root, []int64{111}); err == nil {
		t.Fatal("Run with source == destination: expected an error")
	}
	sameViaDot := filepath.Join(root, ".")
	if err := Run(root, sameViaDot, []int64{111}); err == nil {
		t.Fatalf("Run with path-equivalent destination %s: expected an error", sameViaDot)
	}
}

// TestParseIDs pins the quote-free id grammar: space-separated and
// comma-separated forms parse to the same set, and duplicates collapse in
// first-seen order.
func TestParseIDs(t *testing.T) {
	space, err := ParseIDs([]string{"111", "222"})
	if err != nil {
		t.Fatal(err)
	}
	comma, err := ParseIDs([]string{"111,222"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(space, comma) || !reflect.DeepEqual(space, []int64{111, 222}) {
		t.Errorf("space form %v and comma form %v must both parse to [111 222]", space, comma)
	}

	got, err := ParseIDs([]string{" 111 , 222 ", "111", ",333,"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []int64{111, 222, 333}) {
		t.Errorf("mixed form = %v, want [111 222 333]", got)
	}

	for _, bad := range []string{"abc", "0", "-5", "12.5", ""} {
		if _, err := ParseIDs([]string{bad}); err == nil {
			t.Errorf("ParseIDs(%q): expected an error", bad)
		}
	}
	if _, err := ParseIDs(nil); err == nil {
		t.Error("ParseIDs with no arguments: expected an error")
	}
	if _, err := ParseIDs([]string{",", " "}); err == nil {
		t.Error("ParseIDs of only separators: expected an error")
	}
}
