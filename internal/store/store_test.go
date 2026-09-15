package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "tgxiv.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// testFileHash is the SHA-256 of the empty input: a real, well-formed digest
// for tests that only need the column round-tripped.
const testFileHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// indexNames lists the non-internal indexes attached to table.
func indexNames(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`
SELECT name FROM sqlite_master
WHERE type='index' AND tbl_name=? AND name NOT LIKE 'sqlite_%'`, table)
	if err != nil {
		t.Fatalf("index names: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index names: %v", err)
	}
	return out
}

// seedDialog creates dialog 100 with content rows for the given ids. Content
// must land before any manifest entry: tasks reference messages.
func seedDialog(t *testing.T, s *Store, ids ...int) {
	t.Helper()
	if err := s.UpsertDialog(Dialog{DialogID: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertContent(100, contentFor(ids...)); err != nil {
		t.Fatal(err)
	}
}

// seedTasks adds pending manifest entries for the ids on top of seedDialog.
func seedTasks(t *testing.T, s *Store, ids ...int) {
	t.Helper()
	if _, err := s.UpsertManifest(100, manifestFor(ids...)); err != nil {
		t.Fatal(err)
	}
}

func contentFor(ids ...int) []ContentRecord {
	var out []ContentRecord
	for _, id := range ids {
		out = append(out, ContentRecord{MsgID: id, Type: "message", Date: id, Raw: `{"ID":` + strconv.Itoa(id) + `}`})
	}
	return out
}

func manifestFor(ids ...int) []Record {
	var out []Record
	for _, id := range ids {
		out = append(out, Record{MsgID: id, Size: int64(id), FileName: "f", MediaType: "photo"})
	}
	return out
}

func TestForeignKeysEnforced(t *testing.T) {
	s := openTemp(t)

	// a message for an unknown dialog must be rejected: this is what proves
	// the DSN pragma reached every pooled connection
	if err := s.UpsertContent(999, []ContentRecord{{MsgID: 1, Type: "message", Raw: "{}"}}); err == nil {
		t.Error("UpsertContent for unknown dialog: expected an FK error, got nil")
	}

	// a task with no matching message must be rejected too
	if err := s.UpsertDialog(Dialog{DialogID: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertManifest(100, []Record{{MsgID: 1, Size: 10}}); err == nil {
		t.Error("UpsertManifest with no content row: expected an FK error, got nil")
	}
}

func TestCompositePrimaryKeyAcrossDialogs(t *testing.T) {
	s := openTemp(t)

	// the same msg_id under two dialogs must coexist in messages and tasks
	for _, id := range []int64{100, 200} {
		if err := s.UpsertDialog(Dialog{DialogID: id}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertContent(id, []ContentRecord{{MsgID: 7, Type: "message", Raw: `{"id":7}`}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpsertManifest(id, []Record{{MsgID: 7, FileName: "shared.jpg", Size: 70, MediaType: "photo"}}); err != nil {
			t.Fatal(err)
		}
	}

	for _, id := range []int64{100, 200} {
		if n, err := s.MessageCount(id); err != nil || n != 1 {
			t.Errorf("MessageCount(%d) = %d, %v; want 1, nil", id, n, err)
		}
		pending, err := s.ListPending(id)
		if err != nil || len(pending) != 1 || pending[0].MsgID != 7 {
			t.Errorf("ListPending(%d) = %v, %v; want one row for msg 7", id, pending, err)
		}
	}

	// advancing one dialog's watermark must leave the other untouched
	if err := s.AdvanceLastMsgID(100, 42); err != nil {
		t.Fatal(err)
	}
	if wm, _ := s.LastMsgID(100); wm != 42 {
		t.Errorf("dialog 100 watermark = %d, want 42", wm)
	}
	if wm, _ := s.LastMsgID(200); wm != 0 {
		t.Errorf("dialog 200 watermark = %d, want 0 (isolation)", wm)
	}
}

func TestUpsertAndListPending(t *testing.T) {
	s := openTemp(t)
	seedDialog(t, s, 1, 2, 3)

	recs := []Record{
		{MsgID: 3, Size: 900, FileName: "big.mp4", MediaType: "document"},
		{MsgID: 1, Size: 100, FileName: "small.jpg", MediaType: "photo"},
		{MsgID: 2, Size: 500, FileName: "mid.mp4", MediaType: "document"},
	}
	added, err := s.UpsertManifest(100, recs)
	if err != nil {
		t.Fatal(err)
	}
	if added != 3 {
		t.Errorf("added = %d, want 3", added)
	}

	// re-upsert is idempotent: no new rows
	added, err = s.UpsertManifest(100, recs)
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Errorf("second added = %d, want 0", added)
	}

	pending, err := s.ListPending(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending = %d, want 3", len(pending))
	}
	// smallest first
	if pending[0].MsgID != 1 || pending[1].MsgID != 2 || pending[2].MsgID != 3 {
		t.Errorf("order = %d,%d,%d, want 1,2,3", pending[0].MsgID, pending[1].MsgID, pending[2].MsgID)
	}

	// another dialog's pending list is invisible here
	if err := s.UpsertDialog(Dialog{DialogID: 200}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertContent(200, []ContentRecord{{MsgID: 1, Raw: "{}"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertManifest(200, []Record{{MsgID: 1, Size: 5}}); err != nil {
		t.Fatal(err)
	}
	if pending, _ = s.ListPending(100); len(pending) != 3 {
		t.Errorf("dialog 100 pending = %d, want 3 (dialog scoping)", len(pending))
	}
}

func TestMarkDoneRemovesFromPending(t *testing.T) {
	s := openTemp(t)
	seedDialog(t, s, 1)
	seedTasks(t, s, 1)

	if err := s.MarkDone(100, 1, 10, "/x/100_1_a.jpg", testFileHash); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.ListPending(100)
	if len(pending) != 0 {
		t.Errorf("pending = %d, want 0", len(pending))
	}
	counts, _ := s.CountsAll()
	if counts[StatusDone] != 1 {
		t.Errorf("done = %d, want 1", counts[StatusDone])
	}
}

// TestMarkDoneStoresFileHash pins the stored form of the digest: lowercase
// hex, stored verbatim, and overwritten by the next MarkDone — never a value
// the pipeline silently clears.
func TestMarkDoneStoresFileHash(t *testing.T) {
	s := openTemp(t)
	seedDialog(t, s, 1)
	seedTasks(t, s, 1)

	hashA := strings.Repeat("ab32", 16) // 64 chars of well-formed lowercase hex
	if err := s.MarkDone(100, 1, 10, "media/100/1_f", hashA); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := s.db.QueryRow(`SELECT file_hash FROM tasks WHERE dialog_id=100 AND msg_id=1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != hashA {
		t.Errorf("file_hash = %q, want %q stored verbatim", got, hashA)
	}

	hashB := strings.Repeat("cd97", 16)
	if err := s.MarkDone(100, 1, 10, "media/100/1_f", hashB); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT file_hash FROM tasks WHERE dialog_id=100 AND msg_id=1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != hashB {
		t.Errorf("file_hash after re-download = %q, want %q (the next MarkDone overwrites)", got, hashB)
	}
}

func TestMarkAttemptFailsAfterMax(t *testing.T) {
	s := openTemp(t)
	seedDialog(t, s, 1)
	seedTasks(t, s, 1)

	const max = 3
	// attempts 1 and 2 keep it pending
	for i := 1; i <= 2; i++ {
		if err := s.MarkAttempt(100, 1, 5, "size mismatch", max); err != nil {
			t.Fatal(err)
		}
		pending, _ := s.ListPending(100)
		if len(pending) != 1 {
			t.Fatalf("after attempt %d: pending = %d, want 1", i, len(pending))
		}
	}
	// third attempt trips it to failed
	if err := s.MarkAttempt(100, 1, 5, "size mismatch", max); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.ListPending(100)
	if len(pending) != 0 {
		t.Errorf("pending = %d, want 0", len(pending))
	}
	failed, _ := s.ListFailed(100)
	if len(failed) != 1 {
		t.Errorf("failed = %d, want 1", len(failed))
	}

	// ResetFailed puts it back
	n, _ := s.ResetFailed(100)
	if n != 1 {
		t.Errorf("reset = %d, want 1", n)
	}
	pending, _ = s.ListPending(100)
	if len(pending) != 1 {
		t.Errorf("after reset pending = %d, want 1", len(pending))
	}
}

func TestUpsertDialogNonClobber(t *testing.T) {
	s := openTemp(t)

	if err := s.UpsertDialog(Dialog{DialogID: 100, Username: "news", Title: "News", Kind: "channel"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceLastMsgID(100, 2913); err != nil {
		t.Fatal(err)
	}

	// a refresh that only knows the title must not wipe username/kind
	if err := s.UpsertDialog(Dialog{DialogID: 100, Title: "News!"}); err != nil {
		t.Fatal(err)
	}
	d, ok, err := s.GetDialog(100)
	if err != nil || !ok {
		t.Fatalf("GetDialog = %v,%v; want present", ok, err)
	}
	if d.Title != "News!" || d.Username != "news" || d.Kind != "channel" {
		t.Errorf("dialog = %+v; want title refreshed, other fields kept", d)
	}
	// and the watermark must survive the refresh
	if d.LastMsgID != 2913 {
		t.Errorf("last_msg_id = %d, want 2913 (refresh must not rewind it)", d.LastMsgID)
	}

	if _, ok, _ := s.GetDialog(4242); ok {
		t.Error("GetDialog(4242) on empty slot = ok, want missing")
	}
}

func TestListDialogsOrder(t *testing.T) {
	s := openTemp(t)
	for _, id := range []int64{300, 100, 200} {
		if err := s.UpsertDialog(Dialog{DialogID: id}); err != nil {
			t.Fatal(err)
		}
	}
	dialogs, err := s.ListDialogs()
	if err != nil {
		t.Fatal(err)
	}
	if len(dialogs) != 3 {
		t.Fatalf("dialogs = %d, want 3", len(dialogs))
	}
	for i, want := range []int64{100, 200, 300} {
		if dialogs[i].DialogID != want {
			t.Errorf("dialogs[%d] = %d, want %d", i, dialogs[i].DialogID, want)
		}
	}
}

func TestUpsertManifestRefreshKeepsStatus(t *testing.T) {
	s := openTemp(t)
	seedDialog(t, s, 1)
	seedTasks(t, s, 1)

	// burn two attempts, then let the file land and be verified
	if err := s.MarkAttempt(100, 1, 4, "short", 5); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAttempt(100, 1, 4, "short", 5); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone(100, 1, 10, "media/100_1_a.jpg", testFileHash); err != nil {
		t.Fatal(err)
	}

	// a later export re-states the manifest: progress, result, and the
	// already-computed hash must all survive, while the names refresh
	if _, err := s.UpsertManifest(100, []Record{{MsgID: 1, FileName: "re:named.jpg", FileDiskName: "re_named.jpg", Size: 10, MediaType: "photo"}}); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts int
	var actualSize int64
	var path, fileHash string
	if err := s.db.QueryRow(`
SELECT status, attempts, actual_size, path, file_hash FROM tasks WHERE dialog_id=100 AND msg_id=1`).
		Scan(&status, &attempts, &actualSize, &path, &fileHash); err != nil {
		t.Fatal(err)
	}
	if status != StatusDone || attempts != 2 || actualSize != 10 || path != "media/100_1_a.jpg" || fileHash != testFileHash {
		t.Errorf("task = %s attempts=%d actual=%d path=%q hash=%q; want done/2/10/media/100_1_a.jpg/%s", status, attempts, actualSize, path, fileHash, testFileHash)
	}
	var name, diskName string
	var size int64
	if err := s.db.QueryRow(`SELECT file_name, file_name_disk, size FROM tasks WHERE dialog_id=100 AND msg_id=1`).Scan(&name, &diskName, &size); err != nil {
		t.Fatal(err)
	}
	if name != "re:named.jpg" || diskName != "re_named.jpg" || size != 10 {
		t.Errorf("manifest fields = %q,%q,%d; want the refreshed values", name, diskName, size)
	}
}

// TestListPendingReturnsDiskName checks the disk name round-trips through the
// manifest: stored on upsert, selected back with the pending set.
func TestListPendingReturnsDiskName(t *testing.T) {
	s := openTemp(t)
	seedDialog(t, s, 1, 2)

	if _, err := s.UpsertManifest(100, []Record{
		{MsgID: 1, FileName: "a/b.jpg", FileDiskName: "a_b.jpg", Size: 5},
		{MsgID: 2, FileName: "n.jpg", Size: 6}, // empty stays empty — identical name
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.ListPending(100)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %v, %v; want two rows", pending, err)
	}
	if pending[0].FileDiskName != "a_b.jpg" {
		t.Errorf("msg 1 FileDiskName = %q, want a_b.jpg", pending[0].FileDiskName)
	}
	if pending[1].FileDiskName != "" {
		t.Errorf("msg 2 FileDiskName = %q, want \"\"", pending[1].FileDiskName)
	}
}

func TestResetTask(t *testing.T) {
	s := openTemp(t)
	seedDialog(t, s, 1, 2)
	seedTasks(t, s, 1, 2)

	// msg 1 done, msg 2 failed at max attempts
	if err := s.MarkDone(100, 1, 10, "/x", testFileHash); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.MarkAttempt(100, 2, 0, "boom", 3); err != nil {
			t.Fatal(err)
		}
	}

	for _, id := range []int{1, 2} {
		ok, err := s.ResetTask(100, id)
		if err != nil || !ok {
			t.Fatalf("ResetTask(%d) = %v,%v; want true,nil", id, ok, err)
		}
		var status string
		var attempts int
		var errMsg, fileHash string
		if err := s.db.QueryRow(`
SELECT status, attempts, error, file_hash FROM tasks WHERE dialog_id=100 AND msg_id=?`, id).
			Scan(&status, &attempts, &errMsg, &fileHash); err != nil {
			t.Fatal(err)
		}
		if status != StatusPending || attempts != 0 || errMsg != "" {
			t.Errorf("task %d after reset = %s,%d,%q; want pending,0,\"\"", id, status, attempts, errMsg)
		}
		// the hash describes the file that WAS there; the reset keeps it and
		// the next MarkDone overwrites it
		if want := map[int]string{1: testFileHash, 2: ""}[id]; fileHash != want {
			t.Errorf("task %d file_hash after reset = %q, want %q (kept)", id, fileHash, want)
		}
	}

	// a task that does not exist resets nothing
	ok, err := s.ResetTask(100, 99)
	if err != nil || ok {
		t.Errorf("ResetTask(missing) = %v,%v; want false,nil", ok, err)
	}
}

func TestCountsAcrossDialogs(t *testing.T) {
	s := openTemp(t)

	// dialog 100: 1 done, 1 pending; dialog 200: 1 failed, 1 pending
	for _, id := range []int64{100, 200} {
		if err := s.UpsertDialog(Dialog{DialogID: id}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertContent(id, contentFor(1, 2)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpsertManifest(id, manifestFor(1, 2)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkDone(100, 1, 1, "/x", testFileHash); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone(200, 1, 1, "/x", testFileHash); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.MarkAttempt(200, 2, 0, "boom", 3); err != nil {
			t.Fatal(err)
		}
	}

	counts, err := s.CountsAll()
	if err != nil {
		t.Fatal(err)
	}
	if counts["total"] != 4 || counts[StatusDone] != 2 || counts[StatusPending] != 1 || counts[StatusFailed] != 1 {
		t.Errorf("CountsAll = %v; want total=4 done=2 pending=1 failed=1", counts)
	}

	per, err := s.CountsByDialog()
	if err != nil {
		t.Fatal(err)
	}
	if len(per) != 2 {
		t.Fatalf("CountsByDialog = %d dialogs, want 2", len(per))
	}
	if per[0].Dialog.DialogID != 100 || per[0].Total != 2 || per[0].Done != 1 || per[0].Pending != 1 || per[0].Failed != 0 {
		t.Errorf("dialog 100 counts = %+v; want total=2 done=1 pending=1 failed=0", per[0])
	}
	if per[1].Dialog.DialogID != 200 || per[1].Total != 2 || per[1].Done != 1 || per[1].Pending != 0 || per[1].Failed != 1 {
		t.Errorf("dialog 200 counts = %+v; want total=2 done=1 pending=0 failed=1", per[1])
	}
}

func TestWatermarkMonotonic(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertDialog(Dialog{DialogID: 100}); err != nil {
		t.Fatal(err)
	}

	if wm, _ := s.LastMsgID(100); wm != 0 {
		t.Errorf("initial watermark = %d, want 0", wm)
	}

	if err := s.AdvanceLastMsgID(100, 2913); err != nil {
		t.Fatal(err)
	}
	if wm, _ := s.LastMsgID(100); wm != 2913 {
		t.Errorf("watermark = %d, want 2913", wm)
	}

	// lower and zero ids never rewind it
	_ = s.AdvanceLastMsgID(100, 2000)
	_ = s.AdvanceLastMsgID(100, 0)
	if wm, _ := s.LastMsgID(100); wm != 2913 {
		t.Errorf("watermark = %d, want 2913 (no rewind)", wm)
	}

	// a higher id advances it
	_ = s.AdvanceLastMsgID(100, 2915)
	if wm, _ := s.LastMsgID(100); wm != 2915 {
		t.Errorf("watermark = %d, want 2915", wm)
	}

	// an unknown dialog is an error, not a silent 0
	if err := s.AdvanceLastMsgID(999, 10); err == nil {
		t.Error("AdvanceLastMsgID on unknown dialog: want an error, got nil")
	}
	if wm, err := s.LastMsgID(999); err != nil || wm != 0 {
		t.Errorf("LastMsgID(unknown) = %d,%v; want 0,nil", wm, err)
	}
}

func TestOpenFreshAppliesV3(t *testing.T) {
	s := openTemp(t)

	// dialogs is the FK root with the watermark column
	dlgCols, err := tableColumns(s.db, "dialogs")
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"dialog_id", "username", "title", "kind", "last_msg_id", "updated_at"} {
		if !dlgCols[col] {
			t.Errorf("dialogs column %q missing", col)
		}
	}
	if dlgCols["namespace"] {
		t.Error("dialogs still has a namespace column; the tdl session namespace is a run-level flag, not per-dialog state")
	}

	// messages is keyed by (dialog_id, msg_id)
	msgCols, err := tableColumns(s.db, "messages")
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"dialog_id", "msg_id", "type", "date", "text", "file", "raw"} {
		if !msgCols[col] {
			t.Errorf("messages column %q missing", col)
		}
	}

	// tasks drops v2's redundant dialog_id-in-downloads date column and
	// carries the 1.0.0 file_* family alongside file_name
	taskCols, err := tableColumns(s.db, "tasks")
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"dialog_id", "msg_id", "file_name", "file_name_disk", "size", "media_type", "status", "attempts", "actual_size", "path", "file_hash", "error", "updated_at"} {
		if !taskCols[col] {
			t.Errorf("tasks column %q missing", col)
		}
	}
	if taskCols["date"] {
		t.Error("tasks still has a date column; the owning message carries it")
	}
	if got := indexNames(t, s.db, "tasks"); len(got) != 1 || got[0] != "idx_tasks_status_size" {
		t.Errorf("tasks indexes = %v, want [idx_tasks_status_size]", got)
	}

	// no meta table at all in v3
	if has, err := tableExists(s.db, "meta"); err != nil || has {
		t.Errorf("meta table exists = %v (%v); want gone", has, err)
	}
}

// v2Schema is the old one-dialog-per-db layout: a messages content table and
// a downloads manifest table, with no dialogs table.
const v2Schema = `
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
);
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);`

// createV2DB writes a db in the old layout with one row, then closes it
// cleanly.
func createV2DB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open v2 db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(v2Schema); err != nil {
		t.Fatalf("v2 schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO messages (msg_id, raw) VALUES (1, '{}')`); err != nil {
		t.Fatalf("seed v2 row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO meta (key, value) VALUES ('channel_id', '100')`); err != nil {
		t.Fatalf("seed v2 meta: %v", err)
	}
}

func TestOpenRejectsV2Layout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tgxiv.sqlite")
	createV2DB(t, path)

	s, err := Open(path)
	if err == nil {
		_ = s.Close()
		t.Fatal("Open on a v2 db: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "no longer supports") {
		t.Errorf("error %q does not say the layout is unsupported", err)
	}
}

// preFreezeV3Schema is the v3 layout exactly as the pre-1.0.0 builds created
// it: dialogs present (so it is not v2) but tasks without the file_name_disk
// and file_hash columns the freeze added.
const preFreezeV3Schema = `
CREATE TABLE dialogs (
    dialog_id   INTEGER PRIMARY KEY,
    username    TEXT NOT NULL DEFAULT '',
    title       TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL DEFAULT '',
    last_msg_id INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE messages (
    dialog_id INTEGER NOT NULL REFERENCES dialogs(dialog_id),
    msg_id    INTEGER NOT NULL,
    type      TEXT    NOT NULL DEFAULT 'message',
    date      INTEGER NOT NULL DEFAULT 0,
    text      TEXT    NOT NULL DEFAULT '',
    file      TEXT    NOT NULL DEFAULT '',
    raw       TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (dialog_id, msg_id)
);
CREATE TABLE tasks (
    dialog_id   INTEGER NOT NULL,
    msg_id      INTEGER NOT NULL,
    file_name   TEXT    NOT NULL DEFAULT '',
    size        INTEGER NOT NULL DEFAULT 0,
    media_type  TEXT    NOT NULL DEFAULT '',
    status      TEXT    NOT NULL DEFAULT 'pending',
    attempts    INTEGER NOT NULL DEFAULT 0,
    actual_size INTEGER NOT NULL DEFAULT 0,
    path        TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (dialog_id, msg_id),
    FOREIGN KEY (dialog_id, msg_id) REFERENCES messages(dialog_id, msg_id)
);`

// TestOpenUpgradesPreFreezeTasks covers the silent one-time upgrade of the
// user's already-converted early v3 databases: on open, the missing columns
// appear via ALTER TABLE, existing rows stay readable with the empty-string
// backfill, the upgraded db accepts new writes, and reopening is a no-op.
func TestOpenUpgradesPreFreezeTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tgxiv.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open pre-freeze db: %v", err)
	}
	if _, err := db.Exec(preFreezeV3Schema); err != nil {
		t.Fatalf("pre-freeze schema: %v", err)
	}
	// a done row with recorded progress: it must survive the upgrade intact
	if _, err := db.Exec(`INSERT INTO dialogs (dialog_id) VALUES (100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages (dialog_id, msg_id, raw) VALUES (100, 1, '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO tasks (dialog_id, msg_id, file_name, status, attempts, actual_size, path)
VALUES (100, 1, 'a.jpg', 'done', 2, 10, 'media/100/1_a.jpg')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-freeze v3 db: %v", err)
	}
	defer func() { _ = s.Close() }()

	cols, err := tableColumns(s.db, "tasks")
	if err != nil {
		t.Fatal(err)
	}
	if !cols["file_name_disk"] || !cols["file_hash"] {
		t.Fatalf("upgraded tasks columns = %v; want file_name_disk and file_hash present", cols)
	}
	var status, diskName, fileHash, taskPath string
	var attempts int
	if err := s.db.QueryRow(`
SELECT status, attempts, file_name_disk, file_hash, path FROM tasks WHERE dialog_id=100 AND msg_id=1`).
		Scan(&status, &attempts, &diskName, &fileHash, &taskPath); err != nil {
		t.Fatal(err)
	}
	if status != StatusDone || attempts != 2 || taskPath != "media/100/1_a.jpg" || diskName != "" || fileHash != "" {
		t.Errorf("row after upgrade = %s,%d,%q,%q,%q; want the recorded progress kept and the columns backfilled to ''",
			status, attempts, diskName, fileHash, taskPath)
	}

	// the upgraded db is fully writable: a fresh manifest row round-trips
	if err := s.UpsertContent(100, []ContentRecord{{MsgID: 2, Type: "message", Raw: `{"id":2}`}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertManifest(100, []Record{{MsgID: 2, FileName: "x:y.jpg", FileDiskName: "x_y.jpg", Size: 5}}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.ListPending(100)
	if err != nil || len(pending) != 1 || pending[0].FileDiskName != "x_y.jpg" {
		t.Fatalf("pending on upgraded db = %v, %v; want msg 2 with its disk name", pending, err)
	}

	// reopen: the columns exist now, so the upgrade must be a silent no-op
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen upgraded db: %v", err)
	}
	defer func() { _ = s2.Close() }()
}

func TestUpsertContentLastWins(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertDialog(Dialog{DialogID: 100}); err != nil {
		t.Fatal(err)
	}

	if err := s.UpsertContent(100, []ContentRecord{
		{MsgID: 1, Type: "message", Date: 100, Text: "hello", Raw: `{"id":1}`},
		{MsgID: 2, Type: "photo", Date: 200, File: "2_photo.jpg", Raw: `{"id":2}`},
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.MessageCount(100); n != 2 {
		t.Errorf("MessageCount = %d, want 2", n)
	}

	// a later record for the same msg_id within one call wins, and a refresh
	// overwrites every field
	if err := s.UpsertContent(100, []ContentRecord{
		{MsgID: 1, Type: "message", Date: 101, Text: "stale", File: "old.bin", Raw: `{"v":"stale"}`},
		{MsgID: 1, Type: "message", Date: 102, Text: "fresh", File: "", Raw: `{"v":"fresh"}`},
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.MessageCount(100); n != 2 {
		t.Errorf("MessageCount = %d, want 2 (refresh adds no row)", n)
	}
	var typ string
	var date int
	var text, file, raw string
	if err := s.db.QueryRow(
		`SELECT type, date, text, file, raw FROM messages WHERE dialog_id = 100 AND msg_id = 1`,
	).Scan(&typ, &date, &text, &file, &raw); err != nil {
		t.Fatal(err)
	}
	if typ != "message" || date != 102 || text != "fresh" || file != "" || raw != `{"v":"fresh"}` {
		t.Errorf("content = %q,%d,%q,%q,%q; want message,102,fresh,,{\"v\":\"fresh\"}", typ, date, text, file, raw)
	}
}
