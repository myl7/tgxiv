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
	// attempts burned and a verified path recorded. FK order first: the dialog
	// row, then the content row, then the manifest entry.
	if err := a.Store().UpsertDialog(store.Dialog{DialogID: 100}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store().UpsertContent(100, []store.ContentRecord{{MsgID: 100, Type: "message"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store().UpsertManifest(100, []store.Record{{MsgID: 100, FileName: "photo.jpg", Size: 50, MediaType: "photo"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store().MarkAttempt(100, 100, 0, "boom", 9); err != nil {
		t.Fatal(err)
	}
	if err := a.Store().MarkAttempt(100, 100, 0, "boom", 9); err != nil {
		t.Fatal(err)
	}
	if err := a.Store().MarkDone(100, 100, 50, "media/100_100_photo.jpg"); err != nil {
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
	db, err := sql.Open("sqlite", filepath.Join(dir, "tgxiv.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var text string
	if err := db.QueryRow(`SELECT text FROM messages WHERE dialog_id = 100 AND msg_id = 100`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "new caption" {
		t.Errorf("content 100 text = %q, want %q (day two wins)", text, "new caption")
	}
	var file string
	if err := db.QueryRow(`SELECT file FROM messages WHERE dialog_id = 100 AND msg_id = 101`).Scan(&file); err != nil {
		t.Fatal(err)
	}
	if file != "" {
		t.Errorf("content 101 file = %q, want a text-only row added by day two", file)
	}

	// watermark advanced to the highest id seen across all snapshots
	if wm, _ := a.Store().LastMsgID(100); wm != 101 {
		t.Errorf("watermark = %d, want 101", wm)
	}

	// the finished download is untouched: still done, same attempts and path
	var status string
	var attempts int
	var path string
	if err := db.QueryRow(`SELECT status, attempts, path FROM tasks WHERE dialog_id = 100 AND msg_id = 100`).Scan(&status, &attempts, &path); err != nil {
		t.Fatal(err)
	}
	if status != store.StatusDone || attempts != 2 || path != "media/100_100_photo.jpg" {
		t.Errorf("task 100 = %s attempts=%d path=%q, want done/2/media/100_100_photo.jpg", status, attempts, path)
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
	dialogs, err := a.Store().ListDialogs()
	if err != nil {
		t.Fatal(err)
	}
	if len(dialogs) != 0 {
		t.Errorf("dialogs = %d, want 0", len(dialogs))
	}
}
