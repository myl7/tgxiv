package migrate

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myl7/tgxiv/internal/archive"
	"github.com/myl7/tgxiv/internal/store"
)

// oldV2Tables is the one-channel-per-directory layout the previous release
// wrote: a messages content table and a downloads manifest with per-file
// progress. oldV2Schema adds the meta table holding the watermark and
// channel id; the split lets a variant omit meta for derivation tests.
const oldV2Tables = `
CREATE TABLE messages (
    msg_id INTEGER PRIMARY KEY,
    type   TEXT    NOT NULL DEFAULT 'message',
    date   INTEGER NOT NULL DEFAULT 0,
    text   TEXT    NOT NULL DEFAULT '',
    file   TEXT    NOT NULL DEFAULT '',
    raw    TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE downloads (
    msg_id      INTEGER PRIMARY KEY,
    dialog_id   INTEGER NOT NULL,
    file_name   TEXT    NOT NULL DEFAULT '',
    size        INTEGER NOT NULL DEFAULT 0,
    media_type  TEXT    NOT NULL DEFAULT '',
    date        INTEGER NOT NULL DEFAULT 0,
    status      TEXT    NOT NULL DEFAULT 'pending',
    attempts    INTEGER NOT NULL DEFAULT 0,
    actual_size INTEGER NOT NULL DEFAULT 0,
    path        TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL DEFAULT 0
);`

const oldV2Schema = oldV2Tables + `
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);`

// createOldDB writes an archive.db with the given schema and seed rows into
// dir, closing it cleanly so the conversion opens it read-only like a real
// old archive left behind by the previous release.
func createOldDB(t *testing.T, dir, schema string, seed func(*sql.DB)) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if seed != nil {
		seed(db)
	}
}

// seedOldArchive fills a synthetic v2 channel dir for dialog 100:
//
//	msg 11 done, media on disk            -> done, file moved, path rewritten
//	msg 12 pending, one attempt, no file  -> pending, path dropped
//	msg 13 failed at max attempts, file   -> promoted done: file at manifest size, history kept
//	msg 14 done but the file is gone      -> downgraded to pending, attempts 0
//	msg 15 download without a message row -> orphan: placeholder content + task
//	msg 16 pending, file on disk          -> promoted done: file at manifest size, history kept
//
// plus the decoys the scan must not touch: a stale .tmp partial, another
// dialog's file, batch.json, and a log file.
func seedOldArchive(t *testing.T, dir string) {
	t.Helper()
	createOldDB(t, dir, oldV2Schema, func(db *sql.DB) {
		msgs := []oldMessage{
			{MsgID: 11, Type: "message", Date: 1700000001, Text: "hello", Raw: `{"id":11}`},
			{MsgID: 12, Type: "message", Date: 1700000002, Raw: `{"id":12}`},
			{MsgID: 13, Type: "message", Date: 1700000003, Raw: `{"id":13}`},
			{MsgID: 14, Type: "message", Date: 1700000004, Raw: `{"id":14}`},
			{MsgID: 16, Type: "message", Date: 1700000006, Raw: `{"id":16}`},
		}
		for _, m := range msgs {
			if _, err := db.Exec(
				`INSERT INTO messages (msg_id, type, date, text, file, raw) VALUES (?, ?, ?, ?, ?, ?)`,
				m.MsgID, m.Type, m.Date, m.Text, m.File, m.Raw); err != nil {
				t.Fatal(err)
			}
		}
		dls := []oldDownload{
			{MsgID: 11, DialogID: 100, FileName: "a.jpg", Size: 3, MediaType: "photo", Date: 1700000001, Status: "done", ActualSize: 3, Path: "media/100_11_a.jpg"},
			{MsgID: 12, DialogID: 100, FileName: "b.mp4", Size: 4, MediaType: "document", Date: 1700000002, Status: "pending", Attempts: 1, Error: "short read"},
			{MsgID: 13, DialogID: 100, FileName: "c.jpg", Size: 5, MediaType: "photo", Date: 1700000003, Status: "failed", Attempts: 3, Error: "expected 5 bytes, got 4"},
			{MsgID: 14, DialogID: 100, FileName: "d.jpg", Size: 6, MediaType: "photo", Date: 1700000004, Status: "done", Attempts: 2, ActualSize: 6, Path: "media/100_14_d.jpg"},
			{MsgID: 15, DialogID: 100, FileName: "e.jpg", Size: 7, MediaType: "photo", Date: 1700000005, Status: "pending"},
			{MsgID: 16, DialogID: 100, FileName: "f.jpg", Size: 4, MediaType: "photo", Date: 1700000006, Status: "pending", Attempts: 2, Error: "partial write"},
		}
		for _, d := range dls {
			if _, err := db.Exec(`
INSERT INTO downloads (msg_id, dialog_id, file_name, size, media_type, date, status, attempts, actual_size, path, error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				d.MsgID, d.DialogID, d.FileName, d.Size, d.MediaType, d.Date, d.Status, d.Attempts, d.ActualSize, d.Path, d.Error); err != nil {
				t.Fatal(err)
			}
		}
		for k, v := range map[string]string{"last_msg_id": "16", "channel_id": "100"} {
			if _, err := db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)`, k, v); err != nil {
				t.Fatal(err)
			}
		}
	})

	media := filepath.Join(dir, "media")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"100_11_a.jpg":     "aaa",
		"100_13_c.jpg":     "ccccc",
		"100_16_f.jpg":     "ffff",
		"100_11_a.jpg.tmp": "par", // stale partial: must stay
		"999_11_z.jpg":     "zzz", // another dialog's file: must stay
	} {
		if err := os.WriteFile(filepath.Join(media, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []struct{ path, content string }{
		{filepath.Join(dir, "export", "20260101-010203.json"), `{}`},
		{filepath.Join(dir, "export", "batch.json"), `{}`},
		{filepath.Join(dir, "logs", "failed-20260101.txt"), "boom\n"},
	} {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.path, []byte(f.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// runMigrateDB executes `migrate db` with args (old dir included) against a
// fixed cfg, returning the command error.
func runMigrateDB(t *testing.T, c archive.Config, args ...string) error {
	t.Helper()
	cmd := newDBCmd(func() (archive.Config, error) { return c, nil })
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(args)
	return cmd.Execute()
}

// taskRow reads one task row of the target db for assertions.
func taskRow(t *testing.T, db *sql.DB, msgID int) (status string, attempts int, actualSize int64, path, errMsg string) {
	t.Helper()
	if err := db.QueryRow(`
SELECT status, attempts, actual_size, path, error FROM tasks WHERE dialog_id = 100 AND msg_id = ?`, msgID).
		Scan(&status, &attempts, &actualSize, &path, &errMsg); err != nil {
		t.Fatalf("task %d: %v", msgID, err)
	}
	return status, attempts, actualSize, path, errMsg
}

func TestMigrateDBConvertsV2Dir(t *testing.T) {
	base := t.TempDir()
	oldDir := filepath.Join(base, "old")
	root := filepath.Join(base, "new")
	seedOldArchive(t, oldDir)

	// no --chat-id: the id is derived from the archive's own manifest (100)
	if err := runMigrateDB(t, archive.Config{Dir: root}, oldDir,
		"--username", "@news", "--title", "News", "--kind", "channel"); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(root, "tgxiv.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// the dialog row: username de-@'d, manual title/kind
	var username, title, kind string
	var wm int
	if err := db.QueryRow(`
SELECT username, title, kind, last_msg_id FROM dialogs WHERE dialog_id = 100`).
		Scan(&username, &title, &kind, &wm); err != nil {
		t.Fatal(err)
	}
	if username != "news" || title != "News" || kind != "channel" {
		t.Errorf("dialog = %q,%q,%q; want news,News,channel", username, title, kind)
	}
	if wm != 16 {
		t.Errorf("watermark = %d, want 16 (old meta last_msg_id)", wm)
	}

	// the marker in the NEW root names the dialog per the --username/--title
	// flags, even though media/100/ did not exist before the conversion
	if b, err := os.ReadFile(filepath.Join(root, "media", "100", "dialog.txt")); err != nil {
		t.Errorf("dialog marker: %v", err)
	} else if want := "dialog_id: 100\ntitle: News\nusername: news\n"; string(b) != want {
		t.Errorf("dialog.txt = %q, want %q", b, want)
	}

	// content: the 5 old rows verbatim plus the synthesized orphan placeholder
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE dialog_id = 100`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Errorf("messages = %d, want 6 (5 verbatim + 1 orphan placeholder)", n)
	}
	var typ string
	var date int
	var text, raw string
	if err := db.QueryRow(`
SELECT type, date, text, raw FROM messages WHERE dialog_id = 100 AND msg_id = 15`).
		Scan(&typ, &date, &text, &raw); err != nil {
		t.Fatal(err)
	}
	if typ != "message" || date != 1700000005 || text != "" || raw != "" {
		t.Errorf("orphan placeholder = %q,%d,%q,%q; want message,1700000005,\"\",\"\"", typ, date, text, raw)
	}
	if err := db.QueryRow(`SELECT text FROM messages WHERE dialog_id = 100 AND msg_id = 11`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "hello" {
		t.Errorf("msg 11 text = %q, want verbatim \"hello\"", text)
	}

	// task states: files present at exactly the manifest size land done with
	// the rewritten path whatever the old status said (attempts kept as
	// history, error cleared); the file-less done is downgraded; pending and
	// failed rows without files keep status, attempts, and error, path dropped
	if s, a, asz, p, e := taskRow(t, db, 11); s != store.StatusDone || a != 0 || asz != 3 || p != "media/100/11_a.jpg" || e != "" {
		t.Errorf("task 11 = %s,%d,%d,%q,%q; want done,0,3,media/100/11_a.jpg,\"\"", s, a, asz, p, e)
	}
	if s, a, asz, p, e := taskRow(t, db, 12); s != store.StatusPending || a != 1 || asz != 0 || p != "" || e != "short read" {
		t.Errorf("task 12 = %s,%d,%d,%q,%q; want pending,1,0,\"\",short read", s, a, asz, p, e)
	}
	if s, a, asz, p, e := taskRow(t, db, 13); s != store.StatusDone || a != 3 || asz != 5 || p != "media/100/13_c.jpg" || e != "" {
		t.Errorf("task 13 = %s,%d,%d,%q,%q; want done,3,5,media/100/13_c.jpg,\"\" (failed promoted: file present)", s, a, asz, p, e)
	}
	if s, a, asz, p, e := taskRow(t, db, 14); s != store.StatusPending || a != 0 || asz != 0 || p != "" || e != "" {
		t.Errorf("task 14 = %s,%d,%d,%q,%q; want pending,0,0,\"\",\"\" (done downgraded: file missing)", s, a, asz, p, e)
	}
	if s, a, asz, p, e := taskRow(t, db, 16); s != store.StatusDone || a != 2 || asz != 4 || p != "media/100/16_f.jpg" || e != "" {
		t.Errorf("task 16 = %s,%d,%d,%q,%q; want done,2,4,media/100/16_f.jpg,\"\" (pending promoted: file present)", s, a, asz, p, e)
	}
	if s, a, _, p, _ := taskRow(t, db, 15); s != store.StatusPending || a != 0 || p != "" {
		t.Errorf("task 15 = %s,%d,%q; want pending,0,\"\" (orphan)", s, a, p)
	}

	// media physically in the per-dialog dir; matched files gone from the old
	// dir, decoys untouched
	for name, want := range map[string]int64{"11_a.jpg": 3, "13_c.jpg": 5, "16_f.jpg": 4} {
		fi, err := os.Stat(filepath.Join(root, "media", "100", name))
		if err != nil {
			t.Fatalf("moved file %s: %v", name, err)
		}
		if fi.Size() != want {
			t.Errorf("moved %s = %d bytes, want %d", name, fi.Size(), want)
		}
	}
	for name := range map[string]bool{"100_11_a.jpg": true, "100_13_c.jpg": true, "100_16_f.jpg": true} {
		if _, err := os.Stat(filepath.Join(oldDir, "media", name)); !os.IsNotExist(err) {
			t.Errorf("old media still holds %s (err=%v); matched files must move out", name, err)
		}
	}
	for name := range map[string]bool{"100_11_a.jpg.tmp": true, "999_11_z.jpg": true} {
		if _, err := os.Stat(filepath.Join(oldDir, "media", name)); err != nil {
			t.Errorf("decoy %s was touched: %v", name, err)
		}
	}

	// export snapshot relocated under the dialog dir; batch.json not; the old
	// dir keeps everything else (archive.db included) as the backup
	if _, err := os.Stat(filepath.Join(root, "export", "100", "20260101-010203.json")); err != nil {
		t.Errorf("export snapshot not relocated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "export", "100", "batch.json")); !os.IsNotExist(err) {
		t.Errorf("batch.json relocated (err=%v); only stamped snapshots move", err)
	}
	for _, name := range []string{"20260101-010203.json", "batch.json"} {
		if _, err := os.Stat(filepath.Join(oldDir, "export", name)); err != nil {
			t.Errorf("old export lost %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "logs", "failed-20260101.txt")); err != nil {
		t.Errorf("log not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(oldDir, "archive.db")); err != nil {
		t.Errorf("archive.db removed from the old dir: %v", err)
	}
}

func TestMigrateDBForceRerunKeepsProgress(t *testing.T) {
	base := t.TempDir()
	oldDir := filepath.Join(base, "old")
	root := filepath.Join(base, "new")
	seedOldArchive(t, oldDir)
	cfg := archive.Config{Dir: root}

	// the first run records the dialog's metadata via the manual flags
	if err := runMigrateDB(t, cfg, oldDir, "--chat-id", "100", "--username", "@news", "--title", "News"); err != nil {
		t.Fatal(err)
	}

	// a re-run without --force is a hard error naming the remedy
	err := runMigrateDB(t, cfg, oldDir, "--chat-id", "100")
	if err == nil || !strings.Contains(err.Error(), "already in this archive") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("re-run without --force = %v; want the 'already in this archive (use --force)' error", err)
	}

	// between-runs progress: the downgraded msg 14 got downloaded and verified
	st, err := store.Open(filepath.Join(root, "tgxiv.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDone(100, 14, 6, "media/100/14_d.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// the --force re-run succeeds and must not rewind that progress
	if err := runMigrateDB(t, cfg, oldDir, "--chat-id", "100", "--force"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "tgxiv.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if s, _, asz, p, _ := taskRow(t, db, 14); s != store.StatusDone || asz != 6 || p != "media/100/14_d.jpg" {
		t.Errorf("task 14 after --force = %s,%d,%q; want done,6,media/100/14_d.jpg (progress kept)", s, asz, p)
	}
	// msg 11's file already sits at the target from run one: kept, not re-moved
	if s, _, asz, p, _ := taskRow(t, db, 11); s != store.StatusDone || asz != 3 || p != "media/100/11_a.jpg" {
		t.Errorf("task 11 after --force = %s,%d,%q; want done,3,media/100/11_a.jpg", s, asz, p)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE dialog_id = 100`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Errorf("messages after --force = %d, want 6 (re-run adds no rows)", n)
	}

	// the re-run passed no metadata flags: the marker must still say what the
	// first run recorded, because the stored row — not the re-run's empties —
	// decides its content
	if b, err := os.ReadFile(filepath.Join(root, "media", "100", "dialog.txt")); err != nil {
		t.Errorf("dialog marker after --force: %v", err)
	} else if want := "dialog_id: 100\ntitle: News\nusername: news\n"; string(b) != want {
		t.Errorf("dialog.txt after --force = %q, want %q", b, want)
	}
}

func TestMigrateDBGuardDialogIDMismatch(t *testing.T) {
	oldDir := filepath.Join(t.TempDir(), "old")
	root := filepath.Join(t.TempDir(), "new")
	createOldDB(t, oldDir, oldV2Schema, func(db *sql.DB) {
		if _, err := db.Exec(`INSERT INTO messages (msg_id, raw) VALUES (1, '{}')`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO downloads (msg_id, dialog_id) VALUES (1, 999)`); err != nil {
			t.Fatal(err)
		}
	})

	err := runMigrateDB(t, archive.Config{Dir: root}, oldDir, "--chat-id", "100")
	if err == nil || !strings.Contains(err.Error(), "999") {
		t.Fatalf("dialog id mismatch = %v; want an error naming the offending id 999", err)
	}
	// the guard fires before the target opens: no half-converted root
	if _, statErr := os.Stat(filepath.Join(root, "tgxiv.sqlite")); !os.IsNotExist(statErr) {
		t.Errorf("target db exists after a guarded run (err=%v)", statErr)
	}
}

func TestMigrateDBGuardV1Layout(t *testing.T) {
	oldDir := filepath.Join(t.TempDir(), "old")
	createOldDB(t, oldDir, `
CREATE TABLE messages (
    dialog_id INTEGER NOT NULL,
    msg_id    INTEGER NOT NULL,
    type      TEXT    NOT NULL DEFAULT 'message',
    date      INTEGER NOT NULL DEFAULT 0,
    text      TEXT    NOT NULL DEFAULT '',
    file      TEXT    NOT NULL DEFAULT '',
    raw       TEXT    NOT NULL DEFAULT ''
);`, nil)

	err := runMigrateDB(t, archive.Config{Dir: t.TempDir()}, oldDir, "--chat-id", "100")
	if err == nil || !strings.Contains(err.Error(), "v1") || !strings.Contains(err.Error(), "previous tgxiv release") {
		t.Fatalf("v1 layout = %v; want an error pointing at the previous release's v1->v2 upgrade", err)
	}
}

func TestMigrateDBGuardMissingArchiveDB(t *testing.T) {
	err := runMigrateDB(t, archive.Config{Dir: t.TempDir()}, t.TempDir(), "--chat-id", "100")
	if err == nil || !strings.Contains(err.Error(), "archive.db") {
		t.Fatalf("missing archive.db = %v; want an error naming it", err)
	}
}

func TestMigrateDBGuardChatID(t *testing.T) {
	oldDir := filepath.Join(t.TempDir(), "old")
	seedOldArchive(t, oldDir)

	// 0 means "derive from the archive" and converts cleanly; only an
	// explicitly negative id is a user mistake
	for _, id := range []string{"-1", "-5"} {
		err := runMigrateDB(t, archive.Config{Dir: t.TempDir()}, oldDir, "--chat-id="+id)
		if err == nil || !strings.Contains(err.Error(), "--chat-id") {
			t.Fatalf("--chat-id %s = %v; want the negative-id validation error", id, err)
		}
	}
}

func TestMigrateDBDerivesMixedDialogIDs(t *testing.T) {
	oldDir := filepath.Join(t.TempDir(), "old")
	root := filepath.Join(t.TempDir(), "new")
	createOldDB(t, oldDir, oldV2Schema, func(db *sql.DB) {
		if _, err := db.Exec(`INSERT INTO messages (msg_id, raw) VALUES (1, '{}'), (2, '{}')`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO downloads (msg_id, dialog_id) VALUES (1, 100), (2, 999)`); err != nil {
			t.Fatal(err)
		}
	})

	err := runMigrateDB(t, archive.Config{Dir: root}, oldDir)
	if err == nil || !strings.Contains(err.Error(), "100") || !strings.Contains(err.Error(), "999") ||
		!strings.Contains(err.Error(), "one dialog per directory") {
		t.Fatalf("mixed dialog ids = %v; want an error naming both ids and the one-dialog rule", err)
	}
	// the guard fires before the target opens: no half-converted root
	if _, statErr := os.Stat(filepath.Join(root, "tgxiv.sqlite")); !os.IsNotExist(statErr) {
		t.Errorf("target db exists after a guarded run (err=%v)", statErr)
	}
}

func TestMigrateDBDerivesChatIDFromMeta(t *testing.T) {
	oldDir := filepath.Join(t.TempDir(), "old")
	root := filepath.Join(t.TempDir(), "new")
	// JSON-era import left the manifest empty; meta still knows the channel
	createOldDB(t, oldDir, oldV2Schema, func(db *sql.DB) {
		if _, err := db.Exec(`INSERT INTO messages (msg_id, raw) VALUES (5, '{}')`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO meta (key, value) VALUES ('channel_id', '200'), ('last_msg_id', '5')`); err != nil {
			t.Fatal(err)
		}
	})

	if err := runMigrateDB(t, archive.Config{Dir: root}, oldDir); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "tgxiv.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var wm int
	if err := db.QueryRow(`SELECT last_msg_id FROM dialogs WHERE dialog_id = 200`).Scan(&wm); err != nil {
		t.Fatalf("dialog 200 (meta channel_id fallback): %v", err)
	}
	if wm != 5 {
		t.Errorf("watermark = %d, want 5", wm)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE dialog_id = 200`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("messages = %d, want 1", n)
	}
}

func TestMigrateDBDerivesChatIDUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
		seed   func(*sql.DB) error
	}{
		{name: "no meta table", schema: oldV2Tables},
		{name: "no channel_id key", schema: oldV2Schema, seed: func(db *sql.DB) error {
			_, err := db.Exec(`INSERT INTO meta (key, value) VALUES ('last_msg_id', '5')`)
			return err
		}},
		{name: "unparseable channel_id", schema: oldV2Schema, seed: func(db *sql.DB) error {
			_, err := db.Exec(`INSERT INTO meta (key, value) VALUES ('channel_id', 'not-a-number')`)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldDir := filepath.Join(t.TempDir(), "old")
			root := filepath.Join(t.TempDir(), "new")
			createOldDB(t, oldDir, tc.schema, func(db *sql.DB) {
				if tc.seed != nil {
					if err := tc.seed(db); err != nil {
						t.Fatal(err)
					}
				}
			})

			err := runMigrateDB(t, archive.Config{Dir: root}, oldDir)
			if err == nil || !strings.Contains(err.Error(), "cannot tell") || !strings.Contains(err.Error(), "--chat-id") {
				t.Fatalf("no id source = %v; want the pass --chat-id explicitly error", err)
			}
			if _, statErr := os.Stat(filepath.Join(root, "tgxiv.sqlite")); !os.IsNotExist(statErr) {
				t.Errorf("target db exists after a guarded run (err=%v)", statErr)
			}
		})
	}
}

func TestMigrateDBPromotesFileAlreadyAtTarget(t *testing.T) {
	base := t.TempDir()
	oldDir := filepath.Join(base, "old")
	root := filepath.Join(base, "new")
	seedOldArchive(t, oldDir)

	// msg 12 stays pending in the old dir (no file there), but a previous
	// run already put its file at the v3 location at exactly the manifest size
	if err := os.MkdirAll(filepath.Join(root, "media", "100"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "media", "100", "12_b.mp4"), []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runMigrateDB(t, archive.Config{Dir: root}, oldDir); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "tgxiv.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if s, a, asz, p, e := taskRow(t, db, 12); s != store.StatusDone || a != 1 || asz != 4 || p != "media/100/12_b.mp4" || e != "" {
		t.Errorf("task 12 = %s,%d,%d,%q,%q; want done,1,4,media/100/12_b.mp4,\"\" (promoted: file already at target)", s, a, asz, p, e)
	}
}
