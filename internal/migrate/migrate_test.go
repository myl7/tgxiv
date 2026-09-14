package migrate

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/myl7/tgxiv/internal/archive"
	"github.com/myl7/tgxiv/internal/store"
)

// stamped snapshots as the JSON-era pipeline kept them: the same photo (100)
// appears in both days with different caption text; day two also adds a
// newer text-only message (101)
const replayDayOneJSON = `{"id":100,"messages":[
 {"id":100,"type":"message","file":"photo.jpg","text":"old caption","raw":{"ID":100,"Message":"old caption","Media":{"Photo":{"ID":7,"Sizes":[{"Type":"x","Size":50}]}}}}
]}`

const replayDayTwoJSON = `{"id":100,"messages":[
 {"id":100,"type":"message","file":"photo.jpg","text":"new caption","raw":{"ID":100,"Message":"new caption","Media":{"Photo":{"ID":7,"Sizes":[{"Type":"x","Size":50}]}}}},
 {"id":101,"type":"message","text":"added later","raw":{"ID":101,"Message":"added later","Media":null}}
]}`

func writeExportFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "export", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReplayExportsNewestSnapshotWins(t *testing.T) {
	dir := t.TempDir()
	a, err := archive.Open(archive.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	writeExportFile(t, dir, "20260101-000000.json", replayDayOneJSON)
	writeExportFile(t, dir, "20260102-000000.json", replayDayTwoJSON)
	// non-stamped files must be ignored even when they hold valid exports
	writeExportFile(t, dir, "batch.json", replayDayTwoJSON)
	writeExportFile(t, dir, "weird.txt", replayDayTwoJSON)

	// the photo was already downloaded before the migration: done, with two
	// attempts burned and a verified path recorded
	if _, err := a.Store().UpsertManifest([]store.Record{{MsgID: 100, DialogID: 100, FileName: "photo.jpg", Size: 50, MediaType: "photo", Date: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store().MarkAttempt(100, 0, "boom", 9); err != nil {
		t.Fatal(err)
	}
	if err := a.Store().MarkAttempt(100, 0, "boom", 9); err != nil {
		t.Fatal(err)
	}
	if err := a.Store().MarkDone(100, 50, "media/100_100_photo.jpg"); err != nil {
		t.Fatal(err)
	}

	files, err := replay(a, filepath.Join(dir, "export"))
	if err != nil {
		t.Fatal(err)
	}
	if files != 2 {
		t.Errorf("files = %d, want 2 (batch.json and weird.txt must be skipped)", files)
	}

	// the newest snapshot wins for the shared id; the text-only row lands too
	c, ok, err := a.Store().ContentMessage(100)
	if err != nil || !ok {
		t.Fatalf("content 100 = ok %v, err %v; want present", ok, err)
	}
	if c.Text != "new caption" {
		t.Errorf("content 100 text = %q, want %q (day two wins)", c.Text, "new caption")
	}
	c, ok, err = a.Store().ContentMessage(101)
	if err != nil || !ok {
		t.Fatalf("content 101 = ok %v, err %v; want present", ok, err)
	}
	if c.Text != "added later" || c.File != "" {
		t.Errorf("content 101 = %+v, want a text-only row added by day two", c)
	}

	// watermark advanced to the highest id seen across all snapshots
	if wm, _ := a.Store().LastMsgID(); wm != 101 {
		t.Errorf("watermark = %d, want 101", wm)
	}

	// the finished download is untouched: still done, same attempts and path
	db, err := sql.Open("sqlite", filepath.Join(dir, "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var status string
	var attempts int
	var path string
	if err := db.QueryRow(`SELECT status, attempts, path FROM downloads WHERE msg_id = 100`).Scan(&status, &attempts, &path); err != nil {
		t.Fatal(err)
	}
	if status != store.StatusDone || attempts != 2 || path != "media/100_100_photo.jpg" {
		t.Errorf("download 100 = %s attempts=%d path=%q, want done/2/media/100_100_photo.jpg", status, attempts, path)
	}

	// snapshots are replayed, not consumed: the files must still be there
	for _, name := range []string{"20260101-000000.json", "20260102-000000.json", "batch.json", "weird.txt"} {
		if _, err := os.Stat(filepath.Join(dir, "export", name)); err != nil {
			t.Errorf("replay removed %s: %v", name, err)
		}
	}
}

func TestReplayExportsNothingToReplay(t *testing.T) {
	dir := t.TempDir()
	a, err := archive.Open(archive.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	// a fresh archive has an empty export/ dir: not an error, nothing to do
	files, err := replay(a, filepath.Join(dir, "export"))
	if err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Errorf("files = %d, want 0", files)
	}
	if n, _ := a.Store().MessageCount(); n != 0 {
		t.Errorf("MessageCount = %d, want 0", n)
	}
}
