// Package store is the SQLite-backed state of an archive root holding MANY
// dialogs. Three tables chain together with real foreign keys, and rows must
// be written in that order: dialogs holds one row per archived chat (channel,
// group, or private conversation), messages holds every message of every
// dialog (media and text-only alike, with the verbatim raw JSON as the
// viewer's source of truth), and tasks holds the media pipeline manifest plus
// per-file status and retry bookkeeping, keyed by the message it belongs to.
// The filesystem stays the final authority on "is the file really there at
// the right size"; the DB records content, intent, progress, attempts, and
// permanent failures, and it is the basis for incremental syncs.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"
)

// Status values for a task row.
const (
	StatusPending = "pending" // not yet downloaded, or awaiting retry
	StatusDone    = "done"    // downloaded and size-verified
	StatusFailed  = "failed"  // gave up after the max attempts
)

// schema is the v3 multi-dialog layout. dialogs is the FK root: messages
// reference it, tasks reference messages, so an import must create the dialog
// row before content rows and content rows before manifest rows. task dates
// are deliberately dropped: the owning message row already carries the date.
const schema = `
CREATE TABLE IF NOT EXISTS dialogs (
    dialog_id   INTEGER PRIMARY KEY,
    username    TEXT NOT NULL DEFAULT '',
    title       TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL DEFAULT '',
    last_msg_id INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS messages (
    dialog_id INTEGER NOT NULL REFERENCES dialogs(dialog_id),
    msg_id    INTEGER NOT NULL,
    type      TEXT    NOT NULL DEFAULT 'message',
    date      INTEGER NOT NULL DEFAULT 0,
    text      TEXT    NOT NULL DEFAULT '',
    file      TEXT    NOT NULL DEFAULT '',
    raw       TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (dialog_id, msg_id)
);

CREATE TABLE IF NOT EXISTS tasks (
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
);
CREATE INDEX IF NOT EXISTS idx_tasks_status_size ON tasks(status, size);`

// Dialog is one archived chat. DialogID is the bare positive Telegram dialog
// id (the form tdl accepts). Username carries no '@' prefix; "" means none.
// Kind is "channel" | "group" | "private", stored leniently with no CHECK so
// odd exports never bounce. LastMsgID is the incremental sync watermark.
type Dialog struct {
	DialogID  int64
	Username  string // no '@' prefix; "" = none
	Title     string
	Kind      string // "channel" | "group" | "private"; lenient, no CHECK
	LastMsgID int    // incremental sync watermark
	UpdatedAt int64
}

// Record is a message's download-relevant manifest entry.
type Record struct {
	MsgID     int
	FileName  string
	Size      int64
	MediaType string
}

// TaskState is a tasks row with its full pipeline state, for one-shot imports
// (the db migration) that must preserve progress rather than insert as pending.
type TaskState struct {
	MsgID      int
	FileName   string
	Size       int64
	MediaType  string
	Status     string // pending | done | failed
	Attempts   int
	ActualSize int64
	Path       string // root-relative, slash form
	Error      string
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

// DialogCounts is a per-dialog rollup of task statuses.
type DialogCounts struct {
	Dialog  Dialog
	Total   int
	Done    int
	Pending int
	Failed  int
}

// Store wraps the archive database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the archive DB at path and applies the v3
// schema. A file that still carries the old one-dialog-per-db v2 layout
// (a messages table but no dialogs table) is rejected: it must be converted
// first with "tgxiv migrate db".
func Open(path string) (*Store, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}

	// Detect the v2 layout before any schema write, so the file is never
	// half-migrated behind the user's back.
	hasMessages, err := tableExists(db, "messages")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("detect schema: %w", err)
	}
	if hasMessages {
		hasDialogs, err := tableExists(db, "dialogs")
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("detect schema: %w", err)
		}
		if !hasDialogs {
			_ = db.Close()
			return nil, fmt.Errorf("%s uses the old one-channel schema; run `tgxiv migrate db` to convert it first", path)
		}
	}

	if _, err = db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// openDB opens the archive db with every per-connection pragma riding in the
// DSN. PRAGMA foreign_keys (and busy_timeout) are per-connection state, and
// database/sql pools connections, so an Exec'd pragma would only have covered
// whichever single pooled connection served it — the DSN form makes the
// driver apply them on every connection it opens. WAL lets reads proceed
// during writes; busy_timeout absorbs lock waits; Ping fails fast on a file
// that cannot be opened or created.
func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + escapeDSNPath(path) +
		"?_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

// escapeDSNPath percent-encodes the bytes sqlite's URI parser treats
// specially in a path segment, so an archive directory containing them still
// resolves. Everything else (slashes, the Windows drive colon) is passed
// through verbatim, exactly like the plain-path form.
func escapeDSNPath(path string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		switch c := path[i]; c {
		case '%', '?', '#':
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xF])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// tableExists reports whether db carries a table by that name.
func tableExists(db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// UpsertDialog inserts the dialog row, or refreshes an existing one. On
// refresh, only the non-empty fields among username/title/kind are applied —
// an empty value never wipes stored data, because not every export knows
// every field. updated_at is always bumped; last_msg_id is never touched
// here, so a metadata refresh can never rewind the sync watermark.
func (s *Store) UpsertDialog(d Dialog) error {
	_, err := s.db.Exec(`
INSERT INTO dialogs (dialog_id, username, title, kind, last_msg_id, updated_at)
VALUES (?, ?, ?, ?, 0, ?)
ON CONFLICT(dialog_id) DO UPDATE SET
    username   = CASE WHEN excluded.username <> '' THEN excluded.username ELSE dialogs.username END,
    title      = CASE WHEN excluded.title    <> '' THEN excluded.title    ELSE dialogs.title END,
    kind       = CASE WHEN excluded.kind     <> '' THEN excluded.kind     ELSE dialogs.kind END,
    updated_at = excluded.updated_at`,
		d.DialogID, d.Username, d.Title, d.Kind, time.Now().Unix())
	return err
}

// GetDialog returns the dialog row; ok is false when no such dialog exists.
func (s *Store) GetDialog(id int64) (Dialog, bool, error) {
	var d Dialog
	err := s.db.QueryRow(`
SELECT dialog_id, username, title, kind, last_msg_id, updated_at
FROM dialogs WHERE dialog_id = ?`, id).
		Scan(&d.DialogID, &d.Username, &d.Title, &d.Kind, &d.LastMsgID, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Dialog{}, false, nil
	}
	if err != nil {
		return Dialog{}, false, err
	}
	return d, true, nil
}

// ListDialogs returns every dialog ordered by id.
func (s *Store) ListDialogs() ([]Dialog, error) {
	rows, err := s.db.Query(`
SELECT dialog_id, username, title, kind, last_msg_id, updated_at
FROM dialogs ORDER BY dialog_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Dialog
	for rows.Next() {
		var d Dialog
		if err := rows.Scan(&d.DialogID, &d.Username, &d.Title, &d.Kind, &d.LastMsgID, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpsertContent inserts or fully refreshes the dialog's content rows in a
// single transaction. Existing rows get every field overwritten, and when
// recs holds the same msg_id more than once the later record wins. The dialog
// row must already exist; the FK on messages enforces that.
func (s *Store) UpsertContent(dialogID int64, recs []ContentRecord) (err error) {
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
INSERT INTO messages (dialog_id, msg_id, type, date, text, file, raw)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(dialog_id, msg_id) DO UPDATE SET
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
		if _, err = stmt.Exec(dialogID, r.MsgID, r.Type, r.Date, r.Text, r.File, r.Raw); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpsertManifest inserts new tasks as pending and refreshes the manifest
// fields (name, size, type) of existing ones without touching their status,
// attempts, or download result. Returns how many rows were newly inserted.
// Runs in a single transaction. The FK on tasks enforces that every message
// already has a content row; callers must not add a redundant pre-check.
func (s *Store) UpsertManifest(dialogID int64, recs []Record) (added int, err error) {
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
	existsStmt, err := tx.Prepare(`SELECT 1 FROM tasks WHERE dialog_id = ? AND msg_id = ?`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = existsStmt.Close() }()

	upStmt, err := tx.Prepare(`
INSERT INTO tasks (dialog_id, msg_id, file_name, size, media_type, status, updated_at)
VALUES (?, ?, ?, ?, ?, 'pending', ?)
ON CONFLICT(dialog_id, msg_id) DO UPDATE SET
    file_name  = excluded.file_name,
    size       = excluded.size,
    media_type = excluded.media_type`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = upStmt.Close() }()

	now := time.Now().Unix()
	for _, r := range recs {
		var one int
		switch scanErr := existsStmt.QueryRow(dialogID, r.MsgID).Scan(&one); scanErr {
		case sql.ErrNoRows:
			added++
		case nil:
			// existing row, manifest refresh only
		default:
			return 0, scanErr
		}
		if _, err = upStmt.Exec(dialogID, r.MsgID, r.FileName, r.Size, r.MediaType, now); err != nil {
			return 0, err
		}
	}

	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return added, nil
}

// ImportTasks inserts the dialog's tasks with their full state in one
// transaction. On (dialog_id,msg_id) conflict the existing row wins entirely —
// a --force re-run must not rewind progress made between runs. The FK on tasks
// enforces that every message already has a content row.
func (s *Store) ImportTasks(dialogID int64, tasks []TaskState) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	stmt, err := tx.Prepare(`
INSERT INTO tasks (dialog_id, msg_id, file_name, size, media_type, status, attempts, actual_size, path, error, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(dialog_id, msg_id) DO NOTHING`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	now := time.Now().Unix()
	for _, t := range tasks {
		if _, err = stmt.Exec(dialogID, t.MsgID, t.FileName, t.Size, t.MediaType,
			t.Status, t.Attempts, t.ActualSize, t.Path, t.Error, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MessageCount returns how many messages the dialog has content rows for,
// text-only ones included.
func (s *Store) MessageCount(dialogID int64) (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE dialog_id = ?`, dialogID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ListPending returns the dialog's pending tasks ordered by size ascending
// (smallest first), then by msg_id for a stable order among equal sizes.
func (s *Store) ListPending(dialogID int64) ([]Record, error) {
	rows, err := s.db.Query(`
SELECT msg_id, file_name, size, media_type
FROM tasks WHERE dialog_id = ? AND status = 'pending'
ORDER BY size ASC, msg_id ASC`, dialogID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.MsgID, &r.FileName, &r.Size, &r.MediaType); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkDone records a successful, size-verified download.
func (s *Store) MarkDone(dialogID int64, msgID int, actualSize int64, path string) error {
	_, err := s.db.Exec(`
UPDATE tasks SET status='done', actual_size=?, path=?, error='', updated_at=?
WHERE dialog_id=? AND msg_id=?`, actualSize, path, time.Now().Unix(), dialogID, msgID)
	return err
}

// MarkAttempt records a failed or unverified attempt. It increments attempts and
// flips the task to 'failed' once attempts reach maxAttempts, otherwise leaves
// it 'pending' for another pass.
func (s *Store) MarkAttempt(dialogID int64, msgID int, actualSize int64, errMsg string, maxAttempts int) error {
	_, err := s.db.Exec(`
UPDATE tasks SET
    attempts = attempts + 1,
    actual_size = ?,
    error = ?,
    status = CASE WHEN attempts + 1 >= ? THEN 'failed' ELSE 'pending' END,
    updated_at = ?
WHERE dialog_id = ? AND msg_id = ?`, actualSize, errMsg, maxAttempts, time.Now().Unix(), dialogID, msgID)
	return err
}

// Attempts returns the attempt count recorded for the dialog's message; ok is
// false when the message has no task row.
func (s *Store) Attempts(dialogID int64, msgID int) (n int, ok bool, err error) {
	err = s.db.QueryRow(`SELECT attempts FROM tasks WHERE dialog_id = ? AND msg_id = ?`, dialogID, msgID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

// ResetTask flips the dialog's task for msgID back to pending with attempts
// zeroed and the error cleared, regardless of its prior status. ok is false
// when no such task row exists.
func (s *Store) ResetTask(dialogID int64, msgID int) (bool, error) {
	res, err := s.db.Exec(`
UPDATE tasks SET status='pending', attempts=0, error=''
WHERE dialog_id=? AND msg_id=?`, dialogID, msgID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountsByDialog returns a per-dialog rollup of task statuses for every
// dialog that has tasks, ordered by dialog id.
func (s *Store) CountsByDialog() ([]DialogCounts, error) {
	rows, err := s.db.Query(`
SELECT d.dialog_id, d.username, d.title, d.kind, d.last_msg_id, d.updated_at,
       COUNT(*),
       COALESCE(SUM(t.status = 'done'), 0),
       COALESCE(SUM(t.status = 'pending'), 0),
       COALESCE(SUM(t.status = 'failed'), 0)
FROM dialogs d JOIN tasks t ON t.dialog_id = d.dialog_id
GROUP BY d.dialog_id
ORDER BY d.dialog_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []DialogCounts
	for rows.Next() {
		var dc DialogCounts
		var total, done, pending, failed int64
		if err := rows.Scan(&dc.Dialog.DialogID, &dc.Dialog.Username, &dc.Dialog.Title, &dc.Dialog.Kind,
			&dc.Dialog.LastMsgID, &dc.Dialog.UpdatedAt,
			&total, &done, &pending, &failed); err != nil {
			return nil, err
		}
		dc.Total, dc.Done, dc.Pending, dc.Failed = int(total), int(done), int(pending), int(failed)
		out = append(out, dc)
	}
	return out, rows.Err()
}

// CountsAll returns task counts per status (pending/done/failed) plus
// "total", rolled up across every dialog in the archive.
func (s *Store) CountsAll() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM tasks GROUP BY status`)
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

// ListFailed returns the dialog's permanently failed tasks for reporting.
func (s *Store) ListFailed(dialogID int64) ([]Record, error) {
	rows, err := s.db.Query(`
SELECT msg_id, file_name, size, media_type
FROM tasks WHERE dialog_id=? AND status='failed' ORDER BY msg_id ASC`, dialogID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.MsgID, &r.FileName, &r.Size, &r.MediaType); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResetFailed flips the dialog's failed tasks back to pending with attempts
// zeroed, so a user can force another round after fixing whatever caused the
// failures.
func (s *Store) ResetFailed(dialogID int64) (int, error) {
	res, err := s.db.Exec(`UPDATE tasks SET status='pending', attempts=0, error='' WHERE dialog_id=? AND status='failed'`, dialogID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ResetFailedAll is ResetFailed across every dialog in the archive.
func (s *Store) ResetFailedAll() (int, error) {
	res, err := s.db.Exec(`UPDATE tasks SET status='pending', attempts=0, error='' WHERE status='failed'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// LastMsgID returns the dialog's incremental watermark, or 0 when the dialog
// is unknown or has never advanced one.
func (s *Store) LastMsgID(dialogID int64) (int, error) {
	var id int
	err := s.db.QueryRow(`SELECT last_msg_id FROM dialogs WHERE dialog_id = ?`, dialogID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// AdvanceLastMsgID raises the dialog's watermark to id when id is higher than
// the current value. Lower or zero ids are ignored, so an empty delta never
// rewinds it. An unknown dialog is an error: FK semantics — importing must
// create the dialog row before advancing its watermark.
func (s *Store) AdvanceLastMsgID(dialogID int64, id int) error {
	if id <= 0 {
		return nil
	}
	cur, err := s.LastMsgID(dialogID)
	if err != nil {
		return err
	}
	if id <= cur {
		return nil
	}
	res, err := s.db.Exec(`UPDATE dialogs SET last_msg_id=? WHERE dialog_id=?`, id, dialogID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("dialog %d not in state; exporting must create it before advancing the watermark", dialogID)
	}
	return nil
}
