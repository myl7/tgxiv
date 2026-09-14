package webui

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"
)

const (
	archiveDBName = "tgxiv.sqlite"
	mediaDirName  = "media"

	defaultPageLimit = 100
	maxPageLimit     = 1000
)

// messagesSQL pages one dialog's content messages newest-first. before = 0
// means from the newest; the ? = 0 arm lets one statement serve both first
// and later pages.
const messagesSQL = `
SELECT msg_id AS id, type, date, file, text, raw
FROM messages
WHERE dialog_id = ? AND type = 'message' AND (? = 0 OR msg_id < ?)
ORDER BY msg_id DESC
LIMIT ?`

// msgIDRe matches the msg id leading the last segment of every resolvable
// /downloads file name: a bare "<msgId>" or a "<msgId>_<anything>" form.
var msgIDRe = regexp.MustCompile(`^([0-9]+)(?:_|$)`)

// errNotResolvable marks a /downloads URL that maps to no media file.
var errNotResolvable = errors.New("media file not resolvable")

// Dialog is one dialog recorded in the archive root's tgxiv.sqlite.
type Dialog struct {
	DialogID     int64  `json:"dialogId"`
	Title        string `json:"title,omitempty"`
	Username     string `json:"username,omitempty"` // no '@' prefix; absent when unknown
	Kind         string `json:"kind"`
	MessageCount int    `json:"messageCount"` // every content row, service messages included
}

// message is one row of a messages page. text and file are omitted when the
// column is empty; raw embeds the parsed raw column, or the literal null when
// it is empty or not JSON.
type message struct {
	ID   int64           `json:"id"`
	Type string          `json:"type"`
	Date int64           `json:"date"`
	Text string          `json:"text,omitempty"`
	File string          `json:"file,omitempty"`
	Raw  json.RawMessage `json:"raw"`
}

// messagesResponse is one page of GET /api/channels/{id}/messages.
type messagesResponse struct {
	DialogID int64     `json:"dialogId"`
	Messages []message `json:"messages"`
	HasMore  bool      `json:"hasMore"`
	OldestID *int64    `json:"oldestId"` // messages[0].id, null on an empty page
}

// server is the viewer state shared by every handler.
type server struct {
	root  string
	cache *dbCache
}

func newServer(root string) *server {
	return &server{root: root, cache: &dbCache{root: root}}
}

// cachedDB is a read-only handle to the archive root's tgxiv.sqlite, reused
// until the file's mtime moves.
type cachedDB struct {
	db      *sql.DB
	modTime time.Time
}

// dbCache caches the one read-only handle to <root>/tgxiv.sqlite. The
// archiver may keep writing the db while the viewer runs, so a handle whose
// mtime moved is replaced; the read-only WAL connection never blocks the
// writer meanwhile.
type dbCache struct {
	root  string
	mu    sync.Mutex
	entry *cachedDB
}

// get returns the cached handle, reopening the db when its mtime changed.
// A replaced handle is closed after unlocking: sql.DB.Close waits for
// in-flight queries first, so a reader never touches a closed handle.
func (c *dbCache) get() (*cachedDB, error) {
	dbPath := filepath.Join(c.root, archiveDBName)
	fi, err := os.Stat(dbPath)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	e := c.entry
	if e != nil && e.modTime.Equal(fi.ModTime()) {
		c.mu.Unlock()
		return e, nil
	}
	fresh, err := openArchiveDB(dbPath, fi.ModTime())
	var old *sql.DB
	if err == nil {
		if e != nil {
			old = e.db
		}
		c.entry = fresh
	}
	c.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if old != nil {
		_ = old.Close()
	}
	return fresh, nil
}

// close closes the cached handle.
func (c *dbCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entry != nil {
		_ = c.entry.db.Close()
		c.entry = nil
	}
}

// openArchiveDB opens the archive's tgxiv.sqlite read-only; busy_timeout
// absorbs any lock contention with the archiver.
func openArchiveDB(dbPath string, modTime time.Time) (*cachedDB, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return nil, err
	}
	// connections open lazily; ping so an unreadable db fails here, not mid-request
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &cachedDB{db: db, modTime: modTime}, nil
}

// listDialogs lists the dialogs recorded in the archive root's tgxiv.sqlite,
// ascending by dialog id.
func (s *server) listDialogs() ([]Dialog, error) {
	e, err := s.cache.get()
	if err != nil {
		return nil, err
	}
	rows, err := e.db.Query(`
SELECT d.dialog_id, d.username, d.title, d.kind,
       (SELECT COUNT(*) FROM messages m WHERE m.dialog_id = d.dialog_id)
FROM dialogs d ORDER BY d.dialog_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	ds := []Dialog{}
	for rows.Next() {
		var d Dialog
		if err := rows.Scan(&d.DialogID, &d.Username, &d.Title, &d.Kind, &d.MessageCount); err != nil {
			return nil, err
		}
		ds = append(ds, d)
	}
	return ds, rows.Err()
}

// CountDialogs reports how many dialogs the archive root's tgxiv.sqlite
// records, for the serve command's startup banner. A missing db is an empty
// archive, not an error.
func CountDialogs(root string) (int, error) {
	s := newServer(root)
	defer s.cache.close()
	ds, err := s.listDialogs()
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return len(ds), nil
}

// dialogExists reports whether the dialogs table records id; unknown dialogs
// 404 instead of paging as an empty channel.
func (e *cachedDB) dialogExists(id int64) (bool, error) {
	var one int
	err := e.db.QueryRow(`SELECT 1 FROM dialogs WHERE dialog_id = ?`, id).Scan(&one)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, err
	}
}

// messages returns one page of a dialog's content messages ascending by id,
// plus whether older messages remain. It fetches limit+1 newest-first rows to
// compute hasMore, drops the extra, and reverses the kept rows.
func (e *cachedDB) messages(dialogID, before int64, limit int) ([]message, bool, error) {
	rows, err := e.db.Query(messagesSQL, dialogID, before, before, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()

	msgs := []message{}
	for rows.Next() {
		var m message
		var text, file, raw string
		if err := rows.Scan(&m.ID, &m.Type, &m.Date, &file, &text, &raw); err != nil {
			return nil, false, err
		}
		m.Text, m.File = text, file
		m.Raw = rawJSON(raw)
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := len(msgs) > limit
	if hasMore {
		msgs = msgs[:limit]
	}
	// fetched newest-first; the viewer walks a page oldest-first
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, hasMore, nil
}

// rawJSON embeds the raw column verbatim when it holds JSON, and the literal
// null otherwise (empty column or foreign text).
func rawJSON(raw string) json.RawMessage {
	if raw == "" || !json.Valid([]byte(raw)) {
		return json.RawMessage("null")
	}
	return json.RawMessage(raw)
}

// resolveMedia locates the media file behind a /downloads/<dialogId>/<rest>
// URL. The last rest segment must lead with the msg id — a bare "<msgId>" or
// a "<msgId>_<anything>" name. The done task row's root-relative path is
// tried first — only its basename is meaningful, joined into
// media/<dialogId>/ where it must resolve to a regular file, which blocks
// stored traversal paths. Failing that, media/<dialogId>/ is scanned for the
// "<msgId>_" prefix, whose trailing underscore keeps msg 1 from matching
// msg 10's files.
func (s *server) resolveMedia(dialogID int64, last string) (string, error) {
	m := msgIDRe.FindStringSubmatch(last)
	if m == nil {
		return "", errNotResolvable
	}
	msgID, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return "", errNotResolvable
	}
	mediaDir := filepath.Join(s.root, mediaDirName, strconv.FormatInt(dialogID, 10))

	if e, err := s.cache.get(); err == nil {
		var stored string
		err := e.db.QueryRow(`
SELECT path FROM tasks WHERE dialog_id = ? AND msg_id = ? AND status = 'done' AND path <> ''`,
			dialogID, msgID).Scan(&stored)
		if err == nil {
			p := filepath.Join(mediaDir, path.Base(strings.ReplaceAll(stored, `\`, "/")))
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				return p, nil
			}
		}
	}

	prefix := strconv.FormatInt(msgID, 10) + "_"
	des, err := os.ReadDir(mediaDir)
	if err != nil {
		return "", errNotResolvable
	}
	for _, de := range des {
		if !strings.HasPrefix(de.Name(), prefix) {
			continue
		}
		p := filepath.Join(mediaDir, de.Name())
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", errNotResolvable
}
