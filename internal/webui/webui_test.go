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

// fixtureSchema mirrors the v2 archive.db layout the archiver writes.
const fixtureSchema = `
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
);

INSERT INTO meta (key, value) VALUES ('schema_version', '2');`

// createArchiveDB creates a v2-layout archive.db and seeds it, closing the
// writer cleanly so the read-only viewer opens a settled file.
func createArchiveDB(t *testing.T, path string, seed func(db *sql.DB)) {
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

// fixtureChannelsDir builds a channels root with:
//   - demo_@100: channel_id 100, five content rows (one service message),
//     download rows covering stale/pending/traversal/empty stored paths
//   - alpha_@200: no channel_id, one content row
//   - stray: a subdir without archive.db, which is not a channel
func fixtureChannelsDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	demoMedia := filepath.Join(root, "demo_@100", mediaDirName)
	if err := os.MkdirAll(demoMedia, 0o755); err != nil {
		t.Fatal(err)
	}
	createArchiveDB(t, filepath.Join(root, "demo_@100", archiveDBName), func(db *sql.DB) {
		exec(t, db, `INSERT INTO meta (key, value) VALUES ('channel_id', '100')`)

		rows := []struct {
			id   int64
			typ  string
			date int64
			text string
			file string
			raw  string
		}{
			{1, "message", 1700000100, "", "", `{"Entities":null}`},
			{2, "service", 1700000200, "svc", "", ""},
			{3, "message", 1700000300, "three", "photo.jpg", `{"Entities":[{"Type":"bold"}]}`},
			{5, "message", 1700000500, "five", "", ""},
			{7, "message", 1700000700, "seven", "", "not json"},
		}
		for _, r := range rows {
			exec(t, db, `INSERT INTO messages (msg_id, type, date, text, file, raw) VALUES (?,?,?,?,?,?)`,
				r.id, r.typ, r.date, r.text, r.file, r.raw)
		}

		downloads := []struct {
			id     int64
			status string
			path   string
		}{
			// done; stored path is a stale absolute one, only the basename counts
			{3, "done", "/stale/old/root/media/100_3_photo.jpg"},
			// done but traversal; must not resolve to anything
			{4, "done", "../../etc/passwd"},
			// done with an empty path; falls back to the media/ prefix scan
			{5, "done", ""},
			// pending; never served, even though the stored basename exists
			{6, "pending", "/stale/old/root/media/100_3_photo.jpg"},
		}
		for _, d := range downloads {
			exec(t, db, `INSERT INTO downloads (msg_id, dialog_id, file_name, size, media_type, date, status, path)
VALUES (?, 100, 'f', 1, 'photo', 0, ?, ?)`, d.id, d.status, d.path)
		}
	})
	for name, content := range map[string]string{
		"100_3_photo.jpg": "0123456789",
		"100_5_x.jpg":     "abc",
	} {
		if err := os.WriteFile(filepath.Join(demoMedia, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.MkdirAll(filepath.Join(root, "alpha_@200"), 0o755); err != nil {
		t.Fatal(err)
	}
	createArchiveDB(t, filepath.Join(root, "alpha_@200", archiveDBName), func(db *sql.DB) {
		exec(t, db, `INSERT INTO messages (msg_id, type, date, text) VALUES (1, 'message', 1700000000, 'only')`)
	})

	if err := os.MkdirAll(filepath.Join(root, "stray"), 0o755); err != nil {
		t.Fatal(err)
	}
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

func TestChannelsListing(t *testing.T) {
	mux := New(fixtureChannelsDir(t))

	var chs []map[string]any
	getJSON(t, mux, "/api/channels", &chs)
	if len(chs) != 2 {
		t.Fatalf("channels = %d, want 2 (the stray dir without archive.db is skipped)", len(chs))
	}

	// sorted by channel name: alpha before demo
	alpha, demo := chs[0], chs[1]
	if alpha["channelName"] != "alpha" || demo["channelName"] != "demo" {
		t.Errorf("order = %s, %s; want alpha, demo", alpha["channelName"], demo["channelName"])
	}

	if demo["dirName"] != "demo_@100" || demo["channelStrId"] != "100" || demo["channelId"] != float64(100) {
		t.Errorf("demo = %v", demo)
	}
	// the count includes the service row
	if demo["messageCount"] != float64(5) {
		t.Errorf("demo messageCount = %v, want 5", demo["messageCount"])
	}

	if alpha["dirName"] != "alpha_@200" || alpha["channelStrId"] != "200" {
		t.Errorf("alpha = %v", alpha)
	}
	if alpha["channelId"] != float64(0) {
		t.Errorf("alpha channelId = %v, want 0 (no channel_id in meta)", alpha["channelId"])
	}
	if alpha["messageCount"] != float64(1) {
		t.Errorf("alpha messageCount = %v, want 1", alpha["messageCount"])
	}
}

func TestChannelsMissingRoot(t *testing.T) {
	rec := doGet(t, New(filepath.Join(t.TempDir(), "nope")), "/api/channels")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want []", got)
	}
}

func TestMessagesPaging(t *testing.T) {
	mux := New(fixtureChannelsDir(t))

	type page struct {
		ChannelID int64            `json:"channelId"`
		Messages  []map[string]any `json:"messages"`
		HasMore   bool             `json:"hasMore"`
		OldestID  *int64           `json:"oldestId"`
	}
	get := func(query string) page {
		var p page
		getJSON(t, mux, "/api/channels/demo_@100/messages"+query, &p)
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
	if p.ChannelID != 100 {
		t.Errorf("channelId = %d, want 100", p.ChannelID)
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
	getJSON(t, mux, "/api/channels/demo_@100/messages?before=1", &body)
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
}

func TestMessagesForbiddenDir(t *testing.T) {
	mux := New(fixtureChannelsDir(t))
	for _, target := range []string{
		"/api/channels/%2E%2E/messages",
		"/api/channels/a%2Fb/messages",
	} {
		rec := doGet(t, mux, target)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s: status = %d, want 403", target, rec.Code)
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: decode: %v", target, err)
		}
		if body["error"] != "Forbidden" {
			t.Errorf("GET %s: error = %v, want Forbidden", target, body["error"])
		}
	}
}

func TestMessagesMissingChannel(t *testing.T) {
	rec := doGet(t, New(fixtureChannelsDir(t)), "/api/channels/nope/messages")
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
	mux := New(fixtureChannelsDir(t))

	// msg 3: the done row stores a stale absolute path; only the basename counts
	rec := doGet(t, mux, "/downloads/demo_@100/100_3_photo.jpg")
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

	// Range requests come via http.ServeContent
	req := httptest.NewRequest(http.MethodGet, "/downloads/demo_@100/100_3_photo.jpg", nil)
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
	if rec := doGet(t, mux, "/downloads/demo_@100/100_6_report.jpg"); rec.Code != http.StatusNotFound {
		t.Errorf("msg 6 status = %d, want 404", rec.Code)
	}

	// msg 4: done but the stored path traverses; resolution stays inside media/
	if rec := doGet(t, mux, "/downloads/demo_@100/100_4_passwd"); rec.Code != http.StatusNotFound {
		t.Errorf("msg 4 status = %d, want 404", rec.Code)
	}

	// msg 5: done with an empty path; the media/ prefix scan finds the file
	rec = doGet(t, mux, "/downloads/demo_@100/100_5_report.jpg")
	if rec.Code != http.StatusOK {
		t.Fatalf("msg 5: status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("msg 5 Content-Type = %q, want image/jpeg", ct)
	}
	if rec.Body.String() != "abc" {
		t.Errorf("msg 5 body = %q", rec.Body.String())
	}

	// a name outside the <channelId>_<msgId>_ scheme resolves to nothing
	if rec := doGet(t, mux, "/downloads/demo_@100/photo.jpg"); rec.Code != http.StatusNotFound {
		t.Errorf("unnamed status = %d, want 404", rec.Code)
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
