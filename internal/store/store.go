// Package store is the SQLite-backed state of an archive: the message manifest
// plus per-message download status and retry bookkeeping. The filesystem stays
// the final authority on "is the file really there at the right size"; the DB
// records intent, progress, attempts, and permanent failures, and it is the
// basis for future incremental syncs.
package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"
)

// Status values for a message row.
const (
	StatusPending = "pending" // not yet downloaded, or awaiting retry
	StatusDone    = "done"    // downloaded and size-verified
	StatusFailed  = "failed"  // gave up after the max attempts
)

const schema = `
CREATE TABLE IF NOT EXISTS messages (
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
CREATE INDEX IF NOT EXISTS idx_messages_status_size ON messages(status, size);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
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

// Store wraps the archive database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the archive DB at path and applies the schema.
func Open(path string) (*Store, error) {
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
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// UpsertManifest inserts new messages as pending and refreshes the manifest
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
	existsStmt, err := tx.Prepare(`SELECT 1 FROM messages WHERE msg_id = ?`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = existsStmt.Close() }()

	upStmt, err := tx.Prepare(`
INSERT INTO messages (msg_id, dialog_id, file_name, size, media_type, date, status, updated_at)
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

// ListPending returns all pending messages ordered by size ascending (smallest
// first), then by msg_id for a stable order among equal sizes.
func (s *Store) ListPending() ([]Record, error) {
	rows, err := s.db.Query(`
SELECT msg_id, dialog_id, file_name, size, media_type, date
FROM messages WHERE status = 'pending'
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
UPDATE messages SET status='done', actual_size=?, path=?, error='', updated_at=?
WHERE msg_id=?`, actualSize, path, time.Now().Unix(), msgID)
	return err
}

// MarkAttempt records a failed or unverified attempt. It increments attempts and
// flips the row to 'failed' once attempts reach maxAttempts, otherwise leaves it
// 'pending' for another pass.
func (s *Store) MarkAttempt(msgID int, actualSize int64, errMsg string, maxAttempts int) error {
	_, err := s.db.Exec(`
UPDATE messages SET
    attempts = attempts + 1,
    actual_size = ?,
    error = ?,
    status = CASE WHEN attempts + 1 >= ? THEN 'failed' ELSE 'pending' END,
    updated_at = ?
WHERE msg_id = ?`, actualSize, errMsg, maxAttempts, time.Now().Unix(), msgID)
	return err
}

// Counts returns the number of rows per status (pending/done/failed) plus
// "total".
func (s *Store) Counts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM messages GROUP BY status`)
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

// ListFailed returns permanently failed messages for reporting.
func (s *Store) ListFailed() ([]Record, error) {
	rows, err := s.db.Query(`
SELECT msg_id, dialog_id, file_name, size, media_type, date
FROM messages WHERE status='failed' ORDER BY msg_id ASC`)
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
	res, err := s.db.Exec(`UPDATE messages SET status='pending', attempts=0, error='' WHERE status='failed'`)
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

// MaxMsgID returns the highest message id in the manifest, or 0 if empty.
func (s *Store) MaxMsgID() (int, error) {
	var id sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(msg_id) FROM messages`).Scan(&id); err != nil {
		return 0, err
	}
	return int(id.Int64), nil
}

// metaKeyLastMsgID is the incremental-sync watermark: the highest message id
// (media or not) seen in any import. Kept separate from the media manifest's max
// id so a run of newest-are-text-only messages still advances the watermark.
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
