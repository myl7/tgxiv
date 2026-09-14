package webui

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"
)

// fixtureSchema mirrors the v3 tgxiv.sqlite layout the archiver writes.
const fixtureSchema = `
CREATE TABLE dialogs (
    dialog_id   INTEGER PRIMARY KEY,
    username    TEXT NOT NULL DEFAULT '',
    title       TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL DEFAULT '',
    namespace   TEXT NOT NULL DEFAULT '',
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
);

CREATE INDEX idx_tasks_status_size ON tasks(status, size);`

// createRootDB creates the archive root's tgxiv.sqlite with the v3 schema
// and seeds it, closing the writer cleanly so the read-only viewer opens a
// settled file.
func createRootDB(t *testing.T, path string, seed func(db *sql.DB)) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(fixtureSchema); err != nil {
		t.Fatalf("fixture schema: %v", err)
	}
	if seed != nil {
		seed(db)
	}
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// fixtureRoot builds an archive root whose tgxiv.sqlite holds:
//   - dialog 100 "Demo Channel" (@demo, channel): five content rows (one
//     service message) and task rows covering stale/pending/traversal/empty
//     stored paths
//   - dialog 200 "alpha" (group, no username): one content row
//   - dialog 300 (private, no title): msgs 1 and 10 with media files, for
//     the msg-id boundary of the media prefix scan
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	writeMedia := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	createRootDB(t, filepath.Join(root, archiveDBName), func(db *sql.DB) {
		exec(t, db, `INSERT INTO dialogs (dialog_id, username, title, kind) VALUES (100, 'demo', 'Demo Channel', 'channel')`)
		exec(t, db, `INSERT INTO dialogs (dialog_id, username, title, kind) VALUES (200, '', 'alpha', 'group')`)
		exec(t, db, `INSERT INTO dialogs (dialog_id, username, title, kind) VALUES (300, '', '', 'private')`)

		rows := []struct {
			dialog int64
			id     int64
			typ    string
			date   int64
			text   string
			file   string
			raw    string
		}{
			{100, 1, "message", 1700000100, "", "", `{"Entities":null}`},
			{100, 2, "service", 1700000200, "svc", "", ""},
			{100, 3, "message", 1700000300, "three", "photo.jpg", `{"Entities":[{"Type":"bold"}]}`},
			{100, 5, "message", 1700000500, "five", "", ""},
			{100, 7, "message", 1700000700, "seven", "", "not json"},
			{200, 1, "message", 1700000000, "only", "", ""},
			{300, 1, "message", 1700000001, "", "", ""},
			{300, 10, "message", 1700000010, "", "", ""},
		}
		for _, r := range rows {
			exec(t, db, `INSERT INTO messages (dialog_id, msg_id, type, date, text, file, raw) VALUES (?,?,?,?,?,?,?)`,
				r.dialog, r.id, r.typ, r.date, r.text, r.file, r.raw)
		}

		tasks := []struct {
			id     int64
			status string
			path   string
		}{
			// done; the stored path points into another dialog's media dir —
			// only the basename counts, joined into media/100/
			{3, "done", "media/999/3_photo.jpg"},
			// done but traversal; must not resolve to anything
			{4, "done", "../../etc/passwd"},
			// done with an empty path; falls back to the media/ prefix scan
			{5, "done", ""},
			// pending; never served, even though the stored basename exists
			{6, "pending", "media/100/3_photo.jpg"},
		}
		for _, d := range tasks {
			exec(t, db, `INSERT INTO tasks (dialog_id, msg_id, file_name, status, path)
VALUES (100, ?, 'f', ?, ?)`, d.id, d.status, d.path)
		}
	})

	writeMedia(filepath.Join(mediaDirName, "100", "3_photo.jpg"), "0123456789")
	writeMedia(filepath.Join(mediaDirName, "100", "5_x.jpg"), "abc")
	writeMedia(filepath.Join(mediaDirName, "300", "1_a.jpg"), "one")
	writeMedia(filepath.Join(mediaDirName, "300", "10_b.jpg"), "ten")
	return root
}

func doGet(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func getJSON(t *testing.T, h http.Handler, target string, out any) {
	t.Helper()
	rec := doGet(t, h, target)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %s", target, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("GET %s: decode: %v", target, err)
	}
}

func TestDialogsListing(t *testing.T) {
	mux := New(fixtureRoot(t))

	var ds []map[string]any
	getJSON(t, mux, "/api/channels", &ds)
	if len(ds) != 3 {
		t.Fatalf("dialogs = %d, want 3", len(ds))
	}

	// ordered ascending by dialog id
	demo, alpha, priv := ds[0], ds[1], ds[2]
	if demo["dialogId"] != float64(100) || alpha["dialogId"] != float64(200) || priv["dialogId"] != float64(300) {
		t.Errorf("order = %v, %v, %v; want dialog ids 100, 200, 300", demo["dialogId"], alpha["dialogId"], priv["dialogId"])
	}

	if demo["title"] != "Demo Channel" || demo["username"] != "demo" || demo["kind"] != "channel" {
		t.Errorf("demo = %v", demo)
	}
	// the count includes the service row
	if demo["messageCount"] != float64(5) {
		t.Errorf("demo messageCount = %v, want 5", demo["messageCount"])
	}

	if alpha["title"] != "alpha" || alpha["kind"] != "group" {
		t.Errorf("alpha = %v", alpha)
	}
	// username is '' and omitted from the JSON
	if _, ok := alpha["username"]; ok {
		t.Errorf("alpha = %v, want no username key", alpha)
	}
	if alpha["messageCount"] != float64(1) {
		t.Errorf("alpha messageCount = %v, want 1", alpha["messageCount"])
	}

	if priv["kind"] != "private" {
		t.Errorf("private = %v", priv)
	}
	if _, ok := priv["title"]; ok {
		t.Errorf("private = %v, want no title key", priv)
	}
	if _, ok := priv["username"]; ok {
		t.Errorf("private = %v, want no username key", priv)
	}
	if priv["messageCount"] != float64(2) {
		t.Errorf("private messageCount = %v, want 2", priv["messageCount"])
	}
}

func TestDialogsMissingDB(t *testing.T) {
	rec := doGet(t, New(t.TempDir()), "/api/channels")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want []", got)
	}
}

func TestCountDialogs(t *testing.T) {
	n, err := CountDialogs(fixtureRoot(t))
	if err != nil {
		t.Fatalf("CountDialogs: %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}

	// a missing db is an empty archive, not an error
	n, err = CountDialogs(t.TempDir())
	if err != nil {
		t.Fatalf("CountDialogs on missing db: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

func TestMessagesPaging(t *testing.T) {
	mux := New(fixtureRoot(t))

	type page struct {
		DialogID int64            `json:"dialogId"`
		Messages []map[string]any `json:"messages"`
		HasMore  bool             `json:"hasMore"`
		OldestID *int64           `json:"oldestId"`
	}
	get := func(query string) page {
		var p page
		getJSON(t, mux, "/api/channels/100/messages"+query, &p)
		return p
	}
	ids := func(p page) []int64 {
		out := make([]int64, len(p.Messages))
		for i, m := range p.Messages {
			out[i] = int64(m["id"].(float64))
		}
		return out
	}

	// first page starts from the newest
	p := get("?limit=2")
	if got := ids(p); !reflect.DeepEqual(got, []int64{5, 7}) {
		t.Errorf("first page ids = %v, want [5 7] ascending", got)
	}
	if !p.HasMore {
		t.Error("first page hasMore = false, want true")
	}
	if p.OldestID == nil || *p.OldestID != 5 {
		t.Errorf("first page oldestId = %v, want 5", p.OldestID)
	}
	if p.DialogID != 100 {
		t.Errorf("dialogId = %d, want 100", p.DialogID)
	}
	// the raw column holds foreign text: the literal null
	if p.Messages[1]["raw"] != nil {
		t.Errorf("msg 7 raw = %v, want null", p.Messages[1]["raw"])
	}

	// before pages strictly older than the id
	p = get("?before=5")
	if got := ids(p); !reflect.DeepEqual(got, []int64{1, 3}) {
		t.Errorf("before=5 ids = %v, want [1 3]", got)
	}
	if p.HasMore {
		t.Error("before=5 hasMore = true, want false")
	}
	if p.OldestID == nil || *p.OldestID != 1 {
		t.Errorf("before=5 oldestId = %v, want 1", p.OldestID)
	}

	// empty text/file columns are omitted; raw embeds the parsed JSON
	m1 := p.Messages[0]
	if _, ok := m1["text"]; ok {
		t.Error("msg 1 has a text key; want it omitted")
	}
	if _, ok := m1["file"]; ok {
		t.Error("msg 1 has a file key; want it omitted")
	}
	raw1, ok := m1["raw"].(map[string]any)
	if !ok {
		t.Errorf("msg 1 raw = %T, want an object", m1["raw"])
	} else if _, ok := raw1["Entities"]; !ok {
		t.Errorf("msg 1 raw = %v, want an Entities key", raw1)
	}

	m3 := p.Messages[1]
	if m3["text"] != "three" || m3["file"] != "photo.jpg" {
		t.Errorf("msg 3 = %v", m3)
	}
	raw3, ok := m3["raw"].(map[string]any)
	if !ok || raw3["Entities"] == nil {
		t.Errorf("msg 3 raw = %v, want an object with Entities", m3["raw"])
	}

	// before the oldest id: an empty page, oldestId null, messages an array
	var body map[string]any
	getJSON(t, mux, "/api/channels/100/messages?before=1", &body)
	if _, ok := body["messages"].([]any); !ok {
		t.Errorf("empty page messages = %T, want a JSON array", body["messages"])
	}
	if body["oldestId"] != nil {
		t.Errorf("empty page oldestId = %v, want null", body["oldestId"])
	}
	if body["hasMore"] != false {
		t.Errorf("empty page hasMore = %v, want false", body["hasMore"])
	}

	// a whole-page fetch reports no more
	p = get("?limit=4")
	if got := ids(p); !reflect.DeepEqual(got, []int64{1, 3, 5, 7}) {
		t.Errorf("limit=4 ids = %v, want [1 3 5 7]", got)
	}
	if p.HasMore {
		t.Error("limit=4 hasMore = true, want false")
	}

	// the service row never pages out
	for _, id := range ids(get("?limit=" + strconv.Itoa(maxPageLimit))) {
		if id == 2 {
			t.Error("service message 2 appeared in a page")
		}
	}

	// another dialog's rows never leak into the page
	for _, id := range ids(get("?limit=" + strconv.Itoa(maxPageLimit))) {
		if id == 10 {
			t.Error("dialog 300's msg 10 appeared in dialog 100's page")
		}
	}
}

func TestMessagesNonNumericDialogID(t *testing.T) {
	mux := New(fixtureRoot(t))
	for _, target := range []string{
		"/api/channels/%2E%2E/messages",
		"/api/channels/a%2Fb/messages",
		"/api/channels/1x/messages",
		"/api/channels/-1/messages",
	} {
		rec := doGet(t, mux, target)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", target, rec.Code)
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: decode: %v", target, err)
		}
		if body["error"] != "Not found" {
			t.Errorf("GET %s: error = %v, want Not found", target, body["error"])
		}
	}
}

func TestMessagesUnknownDialog(t *testing.T) {
	rec := doGet(t, New(fixtureRoot(t)), "/api/channels/999/messages")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "Not found" {
		t.Errorf("error = %v, want Not found", body["error"])
	}
}

func TestDownloadResolution(t *testing.T) {
	mux := New(fixtureRoot(t))

	// msg 3: the done row stores a path under another dialog's media dir;
	// only the basename counts, resolved inside media/100/
	rec := doGet(t, mux, "/downloads/100/3_photo.jpg")
	if rec.Code != http.StatusOK {
		t.Fatalf("msg 3: status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("msg 3 Content-Type = %q, want image/jpeg", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != immutableCacheControl {
		t.Errorf("msg 3 Cache-Control = %q, want %q", cc, immutableCacheControl)
	}
	if rec.Body.String() != "0123456789" {
		t.Errorf("msg 3 body = %q", rec.Body.String())
	}

	// the bare "<msgId>" form resolves to the same file
	if rec := doGet(t, mux, "/downloads/100/3"); rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
		t.Errorf("bare msg 3: status = %d, body = %q", rec.Code, rec.Body.String())
	}

	// Range requests come via http.ServeContent
	req := httptest.NewRequest(http.MethodGet, "/downloads/100/3_photo.jpg", nil)
	req.Header.Set("Range", "bytes=0-3")
	rrec := httptest.NewRecorder()
	mux.ServeHTTP(rrec, req)
	if rrec.Code != http.StatusPartialContent {
		t.Errorf("range status = %d, want 206", rrec.Code)
	}
	if rrec.Body.String() != "0123" {
		t.Errorf("range body = %q, want 0123", rrec.Body.String())
	}

	// msg 6: only a pending row, never served even though its stored basename exists
	if rec := doGet(t, mux, "/downloads/100/6_report.jpg"); rec.Code != http.StatusNotFound {
		t.Errorf("msg 6 status = %d, want 404", rec.Code)
	}

	// msg 4: done but the stored path traverses; resolution stays inside media/100/
	if rec := doGet(t, mux, "/downloads/100/4_passwd"); rec.Code != http.StatusNotFound {
		t.Errorf("msg 4 status = %d, want 404", rec.Code)
	}

	// msg 5: done with an empty path; the media/ prefix scan finds the file
	rec = doGet(t, mux, "/downloads/100/5_report.jpg")
	if rec.Code != http.StatusOK {
		t.Fatalf("msg 5: status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("msg 5 Content-Type = %q, want image/jpeg", ct)
	}
	if rec.Body.String() != "abc" {
		t.Errorf("msg 5 body = %q", rec.Body.String())
	}

	// a name without a leading msg id resolves to nothing
	if rec := doGet(t, mux, "/downloads/100/photo.jpg"); rec.Code != http.StatusNotFound {
		t.Errorf("unnamed status = %d, want 404", rec.Code)
	}

	// a non-numeric dialog id is rejected before anything touches the disk
	if rec := doGet(t, mux, "/downloads/%2E%2E/3_photo.jpg"); rec.Code != http.StatusNotFound {
		t.Errorf("traversal dialog id status = %d, want 404", rec.Code)
	}
	if rec := doGet(t, mux, "/downloads/abc/3_photo.jpg"); rec.Code != http.StatusNotFound {
		t.Errorf("alpha dialog id status = %d, want 404", rec.Code)
	}
}

func TestDownloadMsgIDBoundary(t *testing.T) {
	mux := New(fixtureRoot(t))

	// the trailing underscore of the "<msgId>_" prefix keeps msg 1 from
	// matching msg 10's file, in both the bare and the named form
	cases := []struct {
		target string
		body   string
	}{
		{"/downloads/300/1", "one"},
		{"/downloads/300/1_a.jpg", "one"},
		{"/downloads/300/10", "ten"},
		{"/downloads/300/10_b.jpg", "ten"},
	}
	for _, c := range cases {
		rec := doGet(t, mux, c.target)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", c.target, rec.Code)
			continue
		}
		if rec.Body.String() != c.body {
			t.Errorf("GET %s: body = %q, want %q", c.target, rec.Body.String(), c.body)
		}
	}

	// msg 2 has no media file at all
	if rec := doGet(t, mux, "/downloads/300/2"); rec.Code != http.StatusNotFound {
		t.Errorf("msg 2 status = %d, want 404", rec.Code)
	}
}

func TestStaticName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/", "index.html"},
		{"/index.html", "index.html"},
		{"/_next/static/a/b.js", "_next/static/a/b.js"},
		{"/a/./b/../c", "a/c"},
		{"/..", ""}, // cleaned back to the root; nothing to serve
	}
	for _, c := range cases {
		if got := staticName(c.in); got != c.want {
			t.Errorf("staticName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStaticRoot(t *testing.T) {
	rec := doGet(t, New(t.TempDir()), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / Content-Type = %q, want text/html", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("GET / Cache-Control = %q, want no-store", cc)
	}

	body := rec.Body.Bytes()
	if rec.Header().Get("Content-Encoding") == "gzip" {
		// the packed export is embedded; decode to confirm it is still HTML
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("gunzip /: %v", err)
		}
		body, err = io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gunzip /: %v", err)
		}
	}
	if !bytes.Contains(body, []byte("<html")) && !bytes.Contains(body, []byte("<HTML")) {
		t.Errorf("GET / body is not HTML: %q", body)
	}
}

// TestStaticRawFallback covers the placeholder-only embed of a fresh checkout;
// once the export is packed, index.html.gz wins and the scenario cannot occur.
func TestStaticRawFallback(t *testing.T) {
	if _, err := fs.Stat(distRoot, "index.html.gz"); err == nil {
		t.Skip("dist is packed; the raw placeholder is shadowed by index.html.gz")
	}
	rec := doGet(t, New(t.TempDir()), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("GET / Content-Encoding = %q, want none", rec.Header().Get("Content-Encoding"))
	}
	if !strings.Contains(rec.Body.String(), "make build") {
		t.Errorf("GET / body = %q, want the placeholder page", rec.Body.String())
	}
}

// TestStaticNextImmutable checks the immutable cache header on a packed
// _next/static asset; the names are content-hashed, so one is picked at runtime.
func TestStaticNextImmutable(t *testing.T) {
	var asset string
	_ = fs.WalkDir(distRoot, ".", func(p string, d fs.DirEntry, err error) error {
		if asset == "" && strings.HasPrefix(p, "_next/static/") && strings.HasSuffix(p, ".gz") {
			asset = strings.TrimSuffix(p, ".gz")
		}
		return nil
	})
	if asset == "" {
		t.Skip("dist is not packed; no _next/static assets embedded")
	}

	rec := doGet(t, New(t.TempDir()), "/"+asset)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /%s: status = %d", asset, rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
	if cc := rec.Header().Get("Cache-Control"); cc != immutableCacheControl {
		t.Errorf("Cache-Control = %q, want %q", cc, immutableCacheControl)
	}
}
