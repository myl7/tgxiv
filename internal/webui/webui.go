// Package webui serves the archive viewer over one archive root directory:
// the JSON API reads the root's single tgxiv.sqlite read-only (so the
// archiver can keep writing while the viewer runs), media files stream from
// media/<dialogId>/, and the static export is embedded pre-gzipped at build
// time.
package webui

import (
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// distFS holds the pre-built static export, where each file X is stored as
// X.gz. The "all:" prefix is required: the export contains a "_next" dir,
// which plain embed patterns skip.
//
//go:embed all:dist
var distFS embed.FS

// distRoot is distFS re-rooted at dist/, so embedded names map 1:1 to request
// subpaths. The git-tracked placeholder index.html keeps the embed non-empty
// when the export has not been packed.
var distRoot = func() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err) // unreachable: go:embed guarantees dist/ exists
	}
	return sub
}()

// immutableCacheControl marks responses whose content is addressed by an
// unchanging name, so caches may keep them forever.
const immutableCacheControl = "public, max-age=31536000, immutable"

// New builds the viewer handler for an archive root directory: the dir that
// holds tgxiv.sqlite and the per-dialog media/ tree.
func New(root string) http.Handler {
	s := newServer(root)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/channels", s.handleChannels)
	mux.HandleFunc("GET /api/channels/{id}/messages", s.handleMessages)
	mux.HandleFunc("GET /downloads/{id}/{rest...}", s.handleDownload)
	mux.HandleFunc("GET /", s.handleStatic)
	return mux
}

// handleChannels lists the dialogs recorded in the archive root. A missing
// or unreadable db is an empty listing, not an error: the viewer should
// still come up.
func (s *server) handleChannels(w http.ResponseWriter, r *http.Request) {
	ds, err := s.listDialogs()
	if err != nil {
		ds = []Dialog{}
	}
	writeJSON(w, http.StatusOK, ds)
}

// handleMessages pages one dialog's messages oldest-to-newest; before = 0
// starts from the newest. The {id} path value is the numeric dialog id.
func (s *server) handleMessages(w http.ResponseWriter, r *http.Request) {
	id, ok := parseDialogID(r.PathValue("id"))
	if !ok {
		writeJSONError(w, http.StatusNotFound, "Not found")
		return
	}
	before := int64(queryInt(r, "before", 0))
	limit := queryInt(r, "limit", defaultPageLimit)
	if limit < 1 {
		limit = 1
	} else if limit > maxPageLimit {
		limit = maxPageLimit
	}

	e, err := s.cache.get()
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "Not found")
		return
	}
	exists, err := e.dialogExists(id)
	if err != nil || !exists {
		writeJSONError(w, http.StatusNotFound, "Not found")
		return
	}
	msgs, hasMore, err := e.messages(id, before, limit)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "Not found")
		return
	}

	resp := messagesResponse{DialogID: id, Messages: msgs, HasMore: hasMore}
	if len(msgs) > 0 {
		oldest := msgs[0].ID
		resp.OldestID = &oldest
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDownload serves one media file:
// /downloads/<dialogId>/<msgId>[_<name>]. Anything unresolvable is a
// plain-text 404.
func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := parseDialogID(r.PathValue("id"))
	if !ok {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	rest := r.PathValue("rest")
	last := rest
	if i := strings.LastIndex(rest, "/"); i >= 0 {
		last = rest[i+1:]
	}
	p, err := s.resolveMedia(id, last)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	name := filepath.Base(p)
	w.Header().Set("Content-Type", mediaContentType(name))
	w.Header().Set("Cache-Control", immutableCacheControl)
	// called after the manual Content-Type, so ours stands and Range and
	// conditional requests come for free
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// handleStatic serves the embedded export. Files live pre-gzipped as <name>.gz
// and stream with Content-Encoding: gzip; the raw file is only a fallback for
// the git-tracked placeholder that ships before the export is packed.
func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := staticName(r.URL.Path)
	if name == "" {
		http.NotFound(w, r)
		return
	}

	if f := openDistFile(name + ".gz"); f != nil {
		defer func() { _ = f.Close() }()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", staticContentType(name))
		w.Header().Set("Cache-Control", staticCacheControl(name))
		_, _ = io.Copy(w, f)
		return
	}
	if f := openDistFile(name); f != nil {
		defer func() { _ = f.Close() }()
		w.Header().Set("Content-Type", staticContentType(name))
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.Copy(w, f)
		return
	}
	http.NotFound(w, r)
}

// staticName maps a request path to a name in the embedded export: "/" means
// index.html, and cleaning rejects anything that could escape the root.
func staticName(urlPath string) string {
	p := "/" + strings.TrimPrefix(urlPath, "/")
	if p == "/" {
		p = "/index.html"
	}
	name := strings.TrimPrefix(path.Clean(p), "/")
	if !fs.ValidPath(name) { // rejects "", ".", "..", and any ../ segment
		return ""
	}
	return name
}

// openDistFile opens name inside the embedded export, returning nil for
// anything that is not a regular file.
func openDistFile(name string) fs.File {
	f, err := distRoot.Open(name)
	if err != nil {
		return nil
	}
	if fi, err := f.Stat(); err != nil || fi.IsDir() {
		_ = f.Close()
		return nil
	}
	return f
}

// staticCacheControl caches content-hashed _next/static assets forever;
// everything else (HTML above all) must revalidate.
func staticCacheControl(name string) string {
	if strings.HasPrefix(name, "_next/static/") {
		return immutableCacheControl
	}
	return "no-store"
}

// staticContentTypes maps export file extensions to content types; anything
// else is served as application/octet-stream.
var staticContentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".map":   "application/json",
	".json":  "application/json",
	".svg":   "image/svg+xml",
	".ico":   "image/x-icon",
	".txt":   "text/plain; charset=utf-8",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".woff":  "font/woff",
	".woff2": "font/woff2",
}

func staticContentType(name string) string {
	if ct, ok := staticContentTypes[strings.ToLower(filepath.Ext(name))]; ok {
		return ct
	}
	return "application/octet-stream"
}

// mediaContentTypes maps media file extensions to content types, set before
// ServeContent so it never sniffs a wrong one.
var mediaContentTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".mkv":  "video/x-matroska",
	".mp3":  "audio/mpeg",
	".m4a":  "audio/mp4",
	".aac":  "audio/aac",
	".wav":  "audio/wav",
	".ogg":  "audio/ogg",
	".flac": "audio/flac",
	".pdf":  "application/pdf",
	".zip":  "application/zip",
	".txt":  "text/plain",
}

func mediaContentType(name string) string {
	if ct, ok := mediaContentTypes[strings.ToLower(filepath.Ext(name))]; ok {
		return ct
	}
	return "application/octet-stream"
}

// parseDialogID parses the numeric {id} path value. Dialog ids are bare
// integers, so anything non-numeric is rejected outright — this replaces the
// old traversal check on free-form channel dir names by never letting an
// unparseable value near the filesystem.
func parseDialogID(id string) (int64, bool) {
	if id == "" || strings.Trim(id, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// queryInt reads an int query parameter, falling back to def when absent or
// unparseable.
func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
