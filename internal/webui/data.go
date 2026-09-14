package webui

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"
)

const (
	archiveDBName = "archive.db"
	mediaDirName  = "media"

	defaultPageLimit = 100
	maxPageLimit     = 1000
)

// messagesSQL pages content messages newest-first. before = 0 means from the
// newest; the ? = 0 arm lets one statement serve both first and later pages.
const messagesSQL = `
SELECT msg_id AS id, type, date, file, text, raw
FROM messages
WHERE type = 'message' AND (? = 0 OR msg_id < ?)
ORDER BY msg_id DESC
LIMIT ?`

// channelDirRe splits "<name>_@<id>" channel dir names.
var channelDirRe = regexp.MustCompile(`^(.+?)_@(.+)$`)

// downloadNameRe matches tdl's "<channelId>_<msgId>_" file naming, which every
// /downloads URL must carry to be resolvable.
var downloadNameRe = regexp.MustCompile(`^([0-9]+)_([0-9]+)`)

// errNotResolvable marks a /downloads URL that maps to no media file.
var errNotResolvable = errors.New("media file not resolvable")

// Channel is one channel archive under the channels root dir.
type Channel struct {
	DirName      string `json:"dirName"`
	ChannelName  string `json:"channelName"`
	ChannelStrID string `json:"channelStrId,omitempty"` // absent when the dir name has no _@<id> part
	ChannelID    int64  `json:"channelId"`              // meta channel_id, 0 when missing or invalid
	MessageCount int    `json:"messageCount"`           // every content row, service messages included
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

// messagesResponse is one page of GET /api/channels/{dir}/messages.
type messagesResponse struct {
	ChannelID int64     `json:"channelId"`
	Messages  []message `json:"messages"`
	HasMore   bool      `json:"hasMore"`
	OldestID  *int64    `json:"oldestId"` // messages[0].id, null on an empty page
}

// server is the viewer state shared by every handler.
type server struct {
	channelsDir string
	cache       *dbCache
}

func newServer(channelsDir string) *server {
	return &server{
		channelsDir: channelsDir,
		cache:       &dbCache{channelsDir: channelsDir, entries: map[string]*cachedDB{}},
	}
}

// cachedDB is a read-only handle to one channel's archive.db plus the meta
// channel_id parsed at open; both are reused until the file's mtime moves.
type cachedDB struct {
	db        *sql.DB
	modTime   time.Time
	channelID int64
}

// dbCache holds one cachedDB per channel dir. The archiver may rewrite
// archive.db while the viewer runs, so a handle whose mtime moved is replaced;
// the read-only WAL connection never blocks the writer meanwhile.
type dbCache struct {
	channelsDir string
	mu          sync.Mutex
	entries     map[string]*cachedDB
}

// get returns the cached handle for dir, reopening the db when its mtime
// changed. Replaced handles are closed after unlocking: sql.DB.Close waits
// for in-flight queries first, so a reader never touches a closed handle.
func (c *dbCache) get(dir string) (*cachedDB, error) {
	dbPath := filepath.Join(c.channelsDir, dir, archiveDBName)
	fi, err := os.Stat(dbPath)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	e := c.entries[dir]
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
		c.entries[dir] = fresh
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

// close closes every cached handle.
func (c *dbCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for dir, e := range c.entries {
		_ = e.db.Close()
		delete(c.entries, dir)
	}
}

// openArchiveDB opens archive.db read-only (busy_timeout absorbs any lock
// contention with the archiver) and parses the meta channel_id, which is
// stable for the life of the handle.
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

	e := &cachedDB{db: db, modTime: modTime}
	var v string
	switch err := db.QueryRow(`SELECT value FROM meta WHERE key = 'channel_id'`).Scan(&v); {
	case err == nil:
		if id, perr := strconv.ParseInt(v, 10, 64); perr == nil {
			e.channelID = id
		}
	case err == sql.ErrNoRows:
		// no channel_id recorded; the response falls back to 0
	default:
		_ = db.Close()
		return nil, err
	}
	return e, nil
}

// listChannels lists the channel archives under the root dir, sorted by
// channel name. A dir only counts as a channel when its archive.db opens and
// reads; anything else is skipped silently.
func (s *server) listChannels() ([]Channel, error) {
	des, err := os.ReadDir(s.channelsDir)
	if err != nil {
		return nil, err
	}

	chs := []Channel{}
	for _, de := range des {
		// only real subdirs; a symlinked dir is not a channel
		if !de.IsDir() {
			continue
		}
		e, err := s.cache.get(de.Name())
		if err != nil {
			continue
		}
		n, err := e.messageCount()
		if err != nil {
			continue
		}

		ch := Channel{
			DirName:      de.Name(),
			ChannelName:  de.Name(),
			ChannelID:    e.channelID,
			MessageCount: n,
		}
		if m := channelDirRe.FindStringSubmatch(de.Name()); m != nil {
			ch.ChannelName = m[1]
			ch.ChannelStrID = m[2]
		}
		chs = append(chs, ch)
	}
	sort.Slice(chs, func(i, j int) bool { return chs[i].ChannelName < chs[j].ChannelName })
	return chs, nil
}

// CountChannels reports how many channel archives sit under channelsDir, for
// the serve command's startup banner.
func CountChannels(channelsDir string) (int, error) {
	s := newServer(channelsDir)
	defer s.cache.close()
	chs, err := s.listChannels()
	if err != nil {
		return 0, err
	}
	return len(chs), nil
}

// messageCount counts every content row, service messages included — the
// viewer's channel list shows the archive size, not the pageable subset.
func (e *cachedDB) messageCount() (int, error) {
	var n int
	err := e.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n)
	return n, err
}

// messages returns one page of content messages ascending by id, plus whether
// older messages remain. It fetches limit+1 newest-first rows to compute
// hasMore, drops the extra, and reverses the kept rows.
func (e *cachedDB) messages(before int64, limit int) ([]message, bool, error) {
	rows, err := e.db.Query(messagesSQL, before, before, limit+1)
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

// resolveMedia locates the media file behind a /downloads/<dir>/<rest> URL
// whose last segment starts "<channelId>_<msgId>" (tdl's naming). The done
// download row's path is tried first — only its basename is meaningful, and
// it must resolve to a regular file inside media/, which blocks stored
// traversal paths. Failing that, media/ is scanned for the
// "<channelId>_<msgId>_" prefix.
func (s *server) resolveMedia(dir, last string) (string, error) {
	m := downloadNameRe.FindStringSubmatch(last)
	if m == nil {
		return "", errNotResolvable
	}
	channelStr, msgStr := m[1], m[2]
	msgID, _ := strconv.ParseInt(msgStr, 10, 64)
	mediaDir := filepath.Join(s.channelsDir, dir, mediaDirName)

	if e, err := s.cache.get(dir); err == nil {
		var stored string
		err := e.db.QueryRow(`
SELECT path FROM downloads WHERE msg_id = ? AND status = 'done' AND path <> ''`, msgID).Scan(&stored)
		if err == nil {
			p := filepath.Join(mediaDir, path.Base(strings.ReplaceAll(stored, `\`, "/")))
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				return p, nil
			}
		}
	}

	prefix := channelStr + "_" + msgStr + "_"
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
