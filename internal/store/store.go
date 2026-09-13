// Package store is the SQLite-backed state of an archive, split across two
// tables: messages holds the content of every message (media and text-only
// alike, with the verbatim raw JSON as the viewer's source of truth), and
// downloads holds the media pipeline manifest plus per-file status and retry
// bookkeeping. The filesystem stays the final authority on "is the file really
// there at the right size"; the DB records content, intent, progress,
// attempts, and permanent failures, and it is the basis for future incremental
// syncs.
package store

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"
)

// Status values for a download row.
const (
	StatusPending = "pending" // not yet downloaded, or awaiting retry
	StatusDone    = "done"    // downloaded and size-verified
	StatusFailed  = "failed"  // gave up after the max attempts
)

// schemaVersion is recorded in meta so future migrations can tell layouts
// apart; v1 dbs predate the marker entirely.
const schemaVersion = "2"

// contentSchema is factored out because the v1 -> v2 upgrade recreates the
// content table on its own, inside its transaction.
const contentSchema = `
CREATE TABLE IF NOT EXISTS messages (
    msg_id INTEGER PRIMARY KEY,
    type   TEXT    NOT NULL DEFAULT 'message',
    date   INTEGER NOT NULL DEFAULT 0,
    text   TEXT    NOT NULL DEFAULT '',
    file   TEXT    NOT NULL DEFAULT '',
    raw    TEXT    NOT NULL DEFAULT ''
);`

const schema = contentSchema + `

CREATE TABLE IF NOT EXISTS downloads (
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
CREATE INDEX IF NOT EXISTS idx_downloads_status_size ON downloads(status, size);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

INSERT OR IGNORE INTO meta (key, value) VALUES ('schema_version', '` + schemaVersion + `');
`

// Record is a message's download-relevant manifest entry.
type Record struct {
	MsgID     int
	DialogID  int64
	FileName  string
	Size      int64
	MediaType string
	Date      int
}

// ContentRecord is one message's viewer-facing content, media or text-only.
// Raw holds the verbatim JSON of the message, entities included.
type ContentRecord struct {
	MsgID int
	Type  string
	Date  int
	Text  string
	File  string
	Raw   string
}

// Store wraps the archive database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the archive DB at path and applies the
// schema. A v1 db (single-table layout) is upgraded in place, keeping a
// one-time copy of the original at <path>.v1.bak.
func Open(path string) (*Store, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	// A v1 messages table doubles as the download manifest; detect it before
	// any schema write so the original file can be backed up untouched.
	v1, err := isV1(db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("detect schema: %w", err)
	}
	if v1 {
		// close first: the WAL gets checkpointed, leaving one whole file to copy
		if err = db.Close(); err != nil {
			return nil, err
		}
		if err = backupFile(path, path+".v1.bak"); err != nil {
			return nil, fmt.Errorf("backup v1 db: %w", err)
		}
		if db, err = openDB(path); err != nil {
			return nil, err
		}
		if err = upgradeV1(db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("upgrade v1 db: %w", err)
		}
		return &Store{db: db}, nil
	}
	if _, err = db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// openDB opens the archive db and applies the connection pragmas.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// one writer at a time; WAL lets reads proceed, busy_timeout absorbs waits
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	return db, nil
}

// isV1 reports whether db carries the pre-split v1 layout: a messages table
// that still has the download column dialog_id, and no schema_version marker
// in meta.
func isV1(db *sql.DB) (bool, error) {
	cols, err := tableColumns(db, "messages")
	if err != nil {
		return false, err
	}
	if !cols["dialog_id"] {
		return false, nil // fresh or already v2
	}
	_, versioned, err := metaValue(db, "schema_version")
	if err != nil {
		return false, err
	}
	return !versioned, nil
}

// upgradeV1 migrates a v1 db in place in one transaction: the old messages
// manifest becomes the downloads table, the index it carried over is replaced
// by the properly-named one, a fresh empty content table takes the messages
// name, and the schema version marker lands in meta. Content rows are not
// backfilled; later imports repopulate them.
func upgradeV1(db *sql.DB) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.Exec(`ALTER TABLE messages RENAME TO downloads`); err != nil {
		return err
	}
	// the renamed table keeps its old index, under the pre- or post-rename
	// name depending on the sqlite build; drop whatever index is actually
	// attached so the properly-named one can be created below
	idxRows, err := tx.Query(`
SELECT name FROM sqlite_master
WHERE type='index' AND tbl_name='downloads' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return err
	}
	var idxNames []string
	for idxRows.Next() {
		var name string
		if err = idxRows.Scan(&name); err != nil {
			_ = idxRows.Close()
			return err
		}
		idxNames = append(idxNames, name)
	}
	if err = idxRows.Err(); err != nil {
		_ = idxRows.Close()
		return err
	}
	_ = idxRows.Close()
	for _, name := range idxNames {
		if _, err = tx.Exec(`DROP INDEX IF EXISTS "` + name + `"`); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(contentSchema); err != nil {
		return err
	}
	if _, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_downloads_status_size ON downloads(status, size)`); err != nil {
		return err
	}
	_, err = tx.Exec(`
INSERT INTO meta (key, value) VALUES ('schema_version', ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, schemaVersion)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// backupFile copies src to dst once; an existing dst is left untouched, so a
// rerun never clobbers the earliest backup.
func backupFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if os.IsExist(err) {
		return nil // keep the backup already on disk
	}
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

// tableColumns lists the column names of table.
func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`) // table is a literal we control
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// metaValue is GetMeta without a Store, for use during Open when the schema
// (and possibly the meta table itself) is not guaranteed to exist yet.
func metaValue(db *sql.DB, key string) (string, bool, error) {
	var table string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='meta'`).Scan(&table)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var v string
	err = db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	switch err {
	case sql.ErrNoRows:
		return "", false, nil
	case nil:
		return v, true, nil
	default:
		return "", false, err
	}
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// UpsertManifest inserts new downloads as pending and refreshes the manifest
// fields (dialog, name, size, type, date) of existing ones without touching
// their status, attempts, or download result. Returns how many rows were newly
// inserted. Runs in a single transaction.
func (s *Store) UpsertManifest(recs []Record) (added int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// changes() after an upsert is 1 for insert and 2 for update, so we detect
	// a fresh insert by checking existence first within the tx.
	existsStmt, err := tx.Prepare(`SELECT 1 FROM downloads WHERE msg_id = ?`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = existsStmt.Close() }()

	upStmt, err := tx.Prepare(`
INSERT INTO downloads (msg_id, dialog_id, file_name, size, media_type, date, status, updated_at)
VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)
ON CONFLICT(msg_id) DO UPDATE SET
    dialog_id  = excluded.dialog_id,
    file_name  = excluded.file_name,
    size       = excluded.size,
    media_type = excluded.media_type,
    date       = excluded.date`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = upStmt.Close() }()

	now := time.Now().Unix()
	for _, r := range recs {
		var one int
		switch scanErr := existsStmt.QueryRow(r.MsgID).Scan(&one); scanErr {
		case sql.ErrNoRows:
			added++
		case nil:
			// existing row, manifest refresh only
		default:
			return 0, scanErr
		}
		if _, err = upStmt.Exec(r.MsgID, r.DialogID, r.FileName, r.Size, r.MediaType, r.Date, now); err != nil {
			return 0, err
		}
	}

	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return added, nil
}

// UpsertContent inserts or fully refreshes content rows in a single
// transaction. Existing rows get every field overwritten, and when recs holds
// the same msg_id more than once the later record wins.
func (s *Store) UpsertContent(recs []ContentRecord) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// raw can be sizeable, so one prepared statement serves the whole batch
	stmt, err := tx.Prepare(`
INSERT INTO messages (msg_id, type, date, text, file, raw)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(msg_id) DO UPDATE SET
    type = excluded.type,
    date = excluded.date,
    text = excluded.text,
    file = excluded.file,
    raw  = excluded.raw`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, r := range recs {
		if _, err = stmt.Exec(r.MsgID, r.Type, r.Date, r.Text, r.File, r.Raw); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MessageCount returns how many messages have content rows, text-only ones
// included.
func (s *Store) MessageCount() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ContentMessage returns the stored content row of msgID; ok is false when it
// has no content row.
func (s *Store) ContentMessage(msgID int) (rec ContentRecord, ok bool, err error) {
	err = s.db.QueryRow(`
SELECT msg_id, type, date, text, file, raw FROM messages WHERE msg_id = ?`, msgID).
		Scan(&rec.MsgID, &rec.Type, &rec.Date, &rec.Text, &rec.File, &rec.Raw)
	switch err {
	case sql.ErrNoRows:
		return ContentRecord{}, false, nil
	case nil:
		return rec, true, nil
	default:
		return ContentRecord{}, false, err
	}
}

// ListPending returns all pending downloads ordered by size ascending
// (smallest first), then by msg_id for a stable order among equal sizes.
func (s *Store) ListPending() ([]Record, error) {
	rows, err := s.db.Query(`
SELECT msg_id, dialog_id, file_name, size, media_type, date
FROM downloads WHERE status = 'pending'
ORDER BY size ASC, msg_id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.MsgID, &r.DialogID, &r.FileName, &r.Size, &r.MediaType, &r.Date); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkDone records a successful, size-verified download.
func (s *Store) MarkDone(msgID int, actualSize int64, path string) error {
	_, err := s.db.Exec(`
UPDATE downloads SET status='done', actual_size=?, path=?, error='', updated_at=?
WHERE msg_id=?`, actualSize, path, time.Now().Unix(), msgID)
	return err
}

// MarkAttempt records a failed or unverified attempt. It increments attempts and
// flips the row to 'failed' once attempts reach maxAttempts, otherwise leaves it
// 'pending' for another pass.
func (s *Store) MarkAttempt(msgID int, actualSize int64, errMsg string, maxAttempts int) error {
	_, err := s.db.Exec(`
UPDATE downloads SET
    attempts = attempts + 1,
    actual_size = ?,
    error = ?,
    status = CASE WHEN attempts + 1 >= ? THEN 'failed' ELSE 'pending' END,
    updated_at = ?
WHERE msg_id = ?`, actualSize, errMsg, maxAttempts, time.Now().Unix(), msgID)
	return err
}

// Counts returns the number of download rows per status
// (pending/done/failed) plus "total".
func (s *Store) Counts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM downloads GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int{StatusPending: 0, StatusDone: 0, StatusFailed: 0}
	total := 0
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
		total += n
	}
	out["total"] = total
	return out, rows.Err()
}

// ListFailed returns permanently failed downloads for reporting.
func (s *Store) ListFailed() ([]Record, error) {
	rows, err := s.db.Query(`
SELECT msg_id, dialog_id, file_name, size, media_type, date
FROM downloads WHERE status='failed' ORDER BY msg_id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.MsgID, &r.DialogID, &r.FileName, &r.Size, &r.MediaType, &r.Date); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResetFailed flips all failed rows back to pending with attempts zeroed, so a
// user can force another round after fixing whatever caused the failures.
func (s *Store) ResetFailed() (int, error) {
	res, err := s.db.Exec(`UPDATE downloads SET status='pending', attempts=0, error='' WHERE status='failed'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SetMeta stores a key/value metadata pair.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// GetMeta reads a metadata value; ok is false when the key is absent.
func (s *Store) GetMeta(key string) (value string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	switch err {
	case sql.ErrNoRows:
		return "", false, nil
	case nil:
		return value, true, nil
	default:
		return "", false, err
	}
}

// MaxMsgID returns the highest message id in the download manifest, or 0 if
// empty.
func (s *Store) MaxMsgID() (int, error) {
	var id sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(msg_id) FROM downloads`).Scan(&id); err != nil {
		return 0, err
	}
	return int(id.Int64), nil
}

// metaKeyLastMsgID is the incremental-sync watermark: the highest message id
// (media or not) seen in any import. Kept separate from the download
// manifest's max id so a run of newest-are-text-only messages still advances
// the watermark.
const metaKeyLastMsgID = "last_msg_id"

// LastMsgID returns the stored incremental watermark, or 0 if never set.
func (s *Store) LastMsgID() (int, error) {
	v, ok, err := s.GetMeta(metaKeyLastMsgID)
	if err != nil || !ok {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("bad %s %q: %w", metaKeyLastMsgID, v, err)
	}
	return n, nil
}

// AdvanceLastMsgID raises the watermark to id when id is higher than the current
// value. Lower or zero ids are ignored, so an empty delta never rewinds it.
func (s *Store) AdvanceLastMsgID(id int) error {
	if id <= 0 {
		return nil
	}
	cur, err := s.LastMsgID()
	if err != nil {
		return err
	}
	if id <= cur {
		return nil
	}
	return s.SetMeta(metaKeyLastMsgID, strconv.Itoa(id))
}
