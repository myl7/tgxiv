package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

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

func TestUpsertAndListPending(t *testing.T) {
	s := openTemp(t)

	recs := []Record{
		{MsgID: 3, DialogID: 100, Size: 900, FileName: "big.mp4", MediaType: "document"},
		{MsgID: 1, DialogID: 100, Size: 100, FileName: "small.jpg", MediaType: "photo"},
		{MsgID: 2, DialogID: 100, Size: 500, FileName: "mid.mp4", MediaType: "document"},
	}
	added, err := s.UpsertManifest(recs)
	if err != nil {
		t.Fatal(err)
	}
	if added != 3 {
		t.Errorf("added = %d, want 3", added)
	}

	// re-upsert is idempotent: no new rows
	added, err = s.UpsertManifest(recs)
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Errorf("second added = %d, want 0", added)
	}

	pending, err := s.ListPending()
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
}

func TestMarkDoneRemovesFromPending(t *testing.T) {
	s := openTemp(t)
	_, _ = s.UpsertManifest([]Record{{MsgID: 1, DialogID: 1, Size: 10}})

	if err := s.MarkDone(1, 10, "/x/1_1_a.jpg"); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.ListPending()
	if len(pending) != 0 {
		t.Errorf("pending = %d, want 0", len(pending))
	}
	counts, _ := s.Counts()
	if counts[StatusDone] != 1 {
		t.Errorf("done = %d, want 1", counts[StatusDone])
	}
}

func TestMarkAttemptFailsAfterMax(t *testing.T) {
	s := openTemp(t)
	_, _ = s.UpsertManifest([]Record{{MsgID: 1, DialogID: 1, Size: 10}})

	const max = 3
	// attempts 1 and 2 keep it pending
	for i := 1; i <= 2; i++ {
		if err := s.MarkAttempt(1, 5, "size mismatch", max); err != nil {
			t.Fatal(err)
		}
		pending, _ := s.ListPending()
		if len(pending) != 1 {
			t.Fatalf("after attempt %d: pending = %d, want 1", i, len(pending))
		}
	}
	// third attempt trips it to failed
	if err := s.MarkAttempt(1, 5, "size mismatch", max); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.ListPending()
	if len(pending) != 0 {
		t.Errorf("pending = %d, want 0", len(pending))
	}
	failed, _ := s.ListFailed()
	if len(failed) != 1 {
		t.Errorf("failed = %d, want 1", len(failed))
	}

	// ResetFailed puts it back
	n, _ := s.ResetFailed()
	if n != 1 {
		t.Errorf("reset = %d, want 1", n)
	}
	pending, _ = s.ListPending()
	if len(pending) != 1 {
		t.Errorf("after reset pending = %d, want 1", len(pending))
	}
}

func TestMetaAndMaxMsgID(t *testing.T) {
	s := openTemp(t)
	if _, ok, _ := s.GetMeta("channel_id"); ok {
		t.Error("expected missing meta")
	}
	_ = s.SetMeta("channel_id", "42")
	v, ok, _ := s.GetMeta("channel_id")
	if !ok || v != "42" {
		t.Errorf("meta = %q,%v", v, ok)
	}

	if id, _ := s.MaxMsgID(); id != 0 {
		t.Errorf("MaxMsgID empty = %d, want 0", id)
	}
	_, _ = s.UpsertManifest([]Record{{MsgID: 7, DialogID: 1}, {MsgID: 99, DialogID: 1}})
	if id, _ := s.MaxMsgID(); id != 99 {
		t.Errorf("MaxMsgID = %d, want 99", id)
	}
}

func TestWatermarkMonotonic(t *testing.T) {
	s := openTemp(t)

	if wm, _ := s.LastMsgID(); wm != 0 {
		t.Errorf("initial watermark = %d, want 0", wm)
	}

	if err := s.AdvanceLastMsgID(2913); err != nil {
		t.Fatal(err)
	}
	if wm, _ := s.LastMsgID(); wm != 2913 {
		t.Errorf("watermark = %d, want 2913", wm)
	}

	// lower and zero ids never rewind it
	_ = s.AdvanceLastMsgID(2000)
	_ = s.AdvanceLastMsgID(0)
	if wm, _ := s.LastMsgID(); wm != 2913 {
		t.Errorf("watermark = %d, want 2913 (no rewind)", wm)
	}

	// a higher id advances it
	_ = s.AdvanceLastMsgID(2915)
	if wm, _ := s.LastMsgID(); wm != 2915 {
		t.Errorf("watermark = %d, want 2915", wm)
	}
}

func TestOpenFreshAppliesV2(t *testing.T) {
	s := openTemp(t)

	v, ok, err := s.GetMeta("schema_version")
	if err != nil || !ok || v != schemaVersion {
		t.Errorf("schema_version = %q,%v,%v, want %q,true,nil", v, ok, err, schemaVersion)
	}

	// messages is the content table: raw/text present, download columns gone
	msgCols, err := tableColumns(s.db, "messages")
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"msg_id", "type", "date", "text", "file", "raw"} {
		if !msgCols[col] {
			t.Errorf("messages column %q missing", col)
		}
	}
	if msgCols["dialog_id"] {
		t.Error("messages still has dialog_id (v1 layout)")
	}

	// downloads carries the manifest shape
	dlCols, err := tableColumns(s.db, "downloads")
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"msg_id", "dialog_id", "size", "status", "attempts", "path"} {
		if !dlCols[col] {
			t.Errorf("downloads column %q missing", col)
		}
	}
	if got := indexNames(t, s.db, "downloads"); len(got) != 1 || got[0] != "idx_downloads_status_size" {
		t.Errorf("downloads indexes = %v, want [idx_downloads_status_size]", got)
	}

	if n, _ := s.MessageCount(); n != 0 {
		t.Errorf("MessageCount = %d, want 0", n)
	}
}

// v1Schema is the pre-split layout: one messages table doubling as manifest.
const v1Schema = `
CREATE TABLE messages (
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
CREATE INDEX idx_messages_status_size ON messages(status, size);

CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);`

// createV1DB writes a db in the old single-table layout with one row per
// status, then closes it cleanly.
func createV1DB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open v1 db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(v1Schema); err != nil {
		t.Fatalf("v1 schema: %v", err)
	}
	rows := []string{
		`INSERT INTO messages (msg_id, dialog_id, file_name, size, media_type, date, status, attempts, actual_size, path, updated_at)
		 VALUES (1, 100, 'a.jpg', 10, 'photo', 1000, 'done', 1, 10, '/x/a.jpg', 1001)`,
		`INSERT INTO messages (msg_id, dialog_id, file_name, size, media_type, date, status, attempts, error, updated_at)
		 VALUES (2, 100, 'b.mp4', 20, 'document', 2000, 'failed', 3, 'boom', 2001)`,
		`INSERT INTO messages (msg_id, dialog_id, file_name, size, media_type, date, status, updated_at)
		 VALUES (3, 100, 'c.jpg', 30, 'photo', 3000, 'pending', 0)`,
	}
	for _, q := range rows {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed v1 row: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO meta (key, value) VALUES ('channel_id', '100')`); err != nil {
		t.Fatalf("seed v1 meta: %v", err)
	}
}

func TestUpgradeFromV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	createV1DB(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open v1 db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// rows moved to downloads with their statuses intact
	counts, err := s.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts[StatusDone] != 1 || counts[StatusFailed] != 1 || counts[StatusPending] != 1 || counts["total"] != 3 {
		t.Errorf("counts = %v, want done=1 failed=1 pending=1 total=3", counts)
	}
	failed, _ := s.ListFailed()
	if len(failed) != 1 || failed[0].MsgID != 2 {
		t.Errorf("failed = %+v, want msg 2", failed)
	}
	if id, _ := s.MaxMsgID(); id != 3 {
		t.Errorf("MaxMsgID = %d, want 3", id)
	}
	// pre-upgrade meta survives
	if v, ok, _ := s.GetMeta("channel_id"); !ok || v != "100" {
		t.Errorf("channel_id = %q,%v, want 100,true", v, ok)
	}

	// the new content table exists and starts empty
	if n, _ := s.MessageCount(); n != 0 {
		t.Errorf("MessageCount = %d, want 0 (no backfill)", n)
	}
	msgCols, err := tableColumns(s.db, "messages")
	if err != nil {
		t.Fatal(err)
	}
	if msgCols["dialog_id"] || !msgCols["raw"] {
		t.Errorf("upgraded messages columns = v1 layout: %v", msgCols)
	}
	// the old index got replaced by the properly-named one
	if got := indexNames(t, s.db, "downloads"); len(got) != 1 || got[0] != "idx_downloads_status_size" {
		t.Errorf("downloads indexes = %v, want [idx_downloads_status_size]", got)
	}

	// the backup keeps the untouched v1 layout and rows
	bak := path + ".v1.bak"
	bakDB, err := sql.Open("sqlite", bak)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer func() { _ = bakDB.Close() }()
	bakCols, err := tableColumns(bakDB, "messages")
	if err != nil {
		t.Fatal(err)
	}
	if !bakCols["dialog_id"] {
		t.Error("backup messages lost dialog_id; want original v1 schema")
	}
	var tables int
	if err := bakDB.QueryRow(`
SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='downloads'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Error("backup already has a downloads table; want pure v1 schema")
	}
	var rows int
	if err := bakDB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Errorf("backup rows = %d, want 3", rows)
	}

	// reopening the upgraded db is a no-op: no second upgrade, no new backup
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bak); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen upgraded db: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		t.Errorf("reopen produced a new backup at %s; want none", bak)
	}
	if v, ok, _ := s2.GetMeta("schema_version"); !ok || v != schemaVersion {
		t.Errorf("schema_version after reopen = %q,%v, want %q,true", v, ok, schemaVersion)
	}
	if n, _ := s2.MessageCount(); n != 0 {
		t.Errorf("MessageCount after reopen = %d, want 0", n)
	}
	if counts, _ := s2.Counts(); counts["total"] != 3 {
		t.Errorf("total after reopen = %d, want 3", counts["total"])
	}
}

func TestContentMessage(t *testing.T) {
	s := openTemp(t)

	if _, ok, err := s.ContentMessage(1); err != nil || ok {
		t.Fatalf("ContentMessage(1) on empty = %v,%v; want missing with no error", ok, err)
	}

	if err := s.UpsertContent([]ContentRecord{
		{MsgID: 1, Type: "message", Date: 5, Text: "hi", File: "f.bin", Raw: `{"id":1}`},
	}); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := s.ContentMessage(1)
	if err != nil || !ok {
		t.Fatalf("ContentMessage(1) = %v,%v; want present", ok, err)
	}
	if want := (ContentRecord{MsgID: 1, Type: "message", Date: 5, Text: "hi", File: "f.bin", Raw: `{"id":1}`}); rec != want {
		t.Errorf("ContentMessage(1) = %+v, want %+v", rec, want)
	}
}

func TestUpsertContentLastWins(t *testing.T) {
	s := openTemp(t)

	if err := s.UpsertContent([]ContentRecord{
		{MsgID: 1, Type: "message", Date: 100, Text: "hello", Raw: `{"id":1}`},
		{MsgID: 2, Type: "photo", Date: 200, File: "2_photo.jpg", Raw: `{"id":2}`},
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.MessageCount(); n != 2 {
		t.Errorf("MessageCount = %d, want 2", n)
	}

	// a later record for the same msg_id within one call wins, and a refresh
	// overwrites every field
	if err := s.UpsertContent([]ContentRecord{
		{MsgID: 1, Type: "message", Date: 101, Text: "stale", File: "old.bin", Raw: `{"v":"stale"}`},
		{MsgID: 1, Type: "message", Date: 102, Text: "fresh", File: "", Raw: `{"v":"fresh"}`},
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.MessageCount(); n != 2 {
		t.Errorf("MessageCount = %d, want 2 (refresh adds no row)", n)
	}
	var typ string
	var date int
	var text, file, raw string
	if err := s.db.QueryRow(
		`SELECT type, date, text, file, raw FROM messages WHERE msg_id = 1`,
	).Scan(&typ, &date, &text, &file, &raw); err != nil {
		t.Fatal(err)
	}
	if typ != "message" || date != 102 || text != "fresh" || file != "" || raw != `{"v":"fresh"}` {
		t.Errorf("content = %q,%d,%q,%q,%q; want message,102,fresh,,{\"v\":\"fresh\"}", typ, date, text, file, raw)
	}
}
