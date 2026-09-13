package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/myl7/tgxiv/internal/store"
	"github.com/myl7/tgxiv/internal/tdlx"

	_ "modernc.org/sqlite" // read downloads rows directly for replay assertions
)

// fakeRunner stands in for the tdl binary. On Download it reads the batch file
// (proving writeBatch is well-formed) and creates a file of the configured size
// for each listed id. Ids absent from writeSize get no file (deleted/missing).
type fakeRunner struct {
	writeSize map[int]int
	calls     int
	failNext  bool // one-shot: next Download returns an infra error and writes nothing

	// export simulation
	exportJSON     string             // written to opts.Output on Export, if set
	lastExportOpts tdlx.ExportOptions // records the last Export call
}

func (f *fakeRunner) Check(context.Context) error { return nil }

func (f *fakeRunner) Export(_ context.Context, o tdlx.ExportOptions) error {
	f.lastExportOpts = o
	if f.exportJSON != "" {
		return os.WriteFile(o.Output, []byte(f.exportJSON), 0o644)
	}
	return nil
}

func (f *fakeRunner) Download(_ context.Context, o tdlx.DownloadOptions) error {
	f.calls++
	if f.failNext {
		f.failNext = false
		return fmt.Errorf("simulated tdl failure")
	}
	b, err := os.ReadFile(o.BatchFile)
	if err != nil {
		return err
	}
	var content batchFileContent
	if err := json.Unmarshal(b, &content); err != nil {
		return err
	}
	for _, m := range content.Messages {
		sz, ok := f.writeSize[m.ID]
		if !ok {
			continue
		}
		name := fmt.Sprintf("%d_%d_file.bin", content.ID, m.ID)
		if err := os.WriteFile(filepath.Join(o.Dir, name), make([]byte, sz), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func setup(t *testing.T, cfg Config, fake *fakeRunner, recs []store.Record) *Archive {
	t.Helper()
	cfg.Dir = t.TempDir()
	a, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	a.runner = fake

	if err := a.store.SetMeta("channel_id", "100"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.UpsertManifest(recs); err != nil {
		t.Fatal(err)
	}
	return a
}

func recsWithSizeEqualsID(ids ...int) []store.Record {
	var out []store.Record
	for _, id := range ids {
		out = append(out, store.Record{MsgID: id, DialogID: 100, Size: int64(id), FileName: "f"})
	}
	return out
}

func TestDownloadHappyPath(t *testing.T) {
	fake := &fakeRunner{writeSize: map[int]int{1: 1, 2: 2, 3: 3}}
	a := setup(t, Config{BatchSize: 2, MaxAttempts: 2}, fake, recsWithSizeEqualsID(1, 2, 3))

	res, err := a.Download(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Done != 3 || res.Failed != 0 {
		t.Errorf("res = %+v, want Done=3 Failed=0", res)
	}
	counts, _ := a.store.Counts()
	if counts[store.StatusDone] != 3 {
		t.Errorf("done = %d, want 3", counts[store.StatusDone])
	}
	// 3 records, batch 2 => 2 batches in a single pass
	if res.Passes != 1 || fake.calls != 2 {
		t.Errorf("passes=%d calls=%d, want passes=1 calls=2", res.Passes, fake.calls)
	}
}

func TestDownloadRetriesThenFails(t *testing.T) {
	// id 3 always lands at the wrong size, so it exhausts attempts and fails
	fake := &fakeRunner{writeSize: map[int]int{1: 1, 2: 2, 3: 999}}
	a := setup(t, Config{BatchSize: 2, MaxAttempts: 2}, fake, recsWithSizeEqualsID(1, 2, 3))

	res, err := a.Download(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Done != 2 || res.Failed != 1 {
		t.Errorf("res = %+v, want Done=2 Failed=1", res)
	}
	if res.Passes != 2 {
		t.Errorf("passes = %d, want 2 (one retry pass for id 3)", res.Passes)
	}
	failed, _ := a.store.ListFailed()
	if len(failed) != 1 || failed[0].MsgID != 3 {
		t.Errorf("failed = %+v, want [id 3]", failed)
	}
}

func TestDownloadInfraErrorDoesNotBurnAttempts(t *testing.T) {
	fake := &fakeRunner{writeSize: map[int]int{1: 1}, failNext: true}
	a := setup(t, Config{BatchSize: 10, MaxAttempts: 3}, fake, recsWithSizeEqualsID(1))

	_, err := a.Download(context.Background())
	if err == nil {
		t.Fatal("expected an error when tdl fails with no progress")
	}
	// the message must remain pending with no attempt consumed
	pending, _ := a.store.ListPending()
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	counts, _ := a.store.Counts()
	if counts[store.StatusFailed] != 0 {
		t.Errorf("failed = %d, want 0 (infra error must not fail the message)", counts[store.StatusFailed])
	}
}

func TestDownloadCanceledStops(t *testing.T) {
	fake := &fakeRunner{writeSize: map[int]int{1: 1}}
	a := setup(t, Config{BatchSize: 10, MaxAttempts: 3}, fake, recsWithSizeEqualsID(1))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	_, err := a.Download(ctx)
	if err == nil {
		t.Fatal("expected context error")
	}
	if fake.calls != 0 {
		t.Errorf("calls = %d, want 0 (should not invoke tdl when already canceled)", fake.calls)
	}
}

// delta export JSON: one new photo (2914) plus a newer text-only message (2915)
const deltaExportJSON = `{"id":100,"messages":[
 {"id":2914,"type":"message","file":"n.jpg","text":"caption","raw":{"ID":2914,"Message":"caption","Media":{"Photo":{"ID":1,"Sizes":[{"Type":"x","Size":50}]}}}},
 {"id":2915,"type":"message","text":"text only","raw":{"ID":2915,"Message":"text only","Media":null}}
]}`

func openBare(t *testing.T, fake *fakeRunner) *Archive {
	t.Helper()
	a, err := Open(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	a.runner = fake
	return a
}

func TestExportIncrementalUsesWatermark(t *testing.T) {
	fake := &fakeRunner{exportJSON: deltaExportJSON}
	a := openBare(t, fake)

	// pretend a previous sync reached message 2913
	if err := a.store.AdvanceLastMsgID(2913); err != nil {
		t.Fatal(err)
	}

	res, err := a.Export(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	// tdl must be asked for id >= 2914 (watermark + 1)
	if fake.lastExportOpts.SinceID != 2914 {
		t.Errorf("SinceID = %d, want 2914", fake.lastExportOpts.SinceID)
	}
	if !res.Incremental || res.SinceID != 2914 {
		t.Errorf("res = %+v, want incremental with SinceID 2914", res)
	}
	if res.Added != 1 {
		t.Errorf("added = %d, want 1 (only the photo)", res.Added)
	}
	// watermark advances to the newest id seen, including the text message
	if wm, _ := a.store.LastMsgID(); wm != 2915 {
		t.Errorf("watermark = %d, want 2915", wm)
	}
	// the new photo is now pending
	pending, _ := a.store.ListPending()
	if len(pending) != 1 || pending[0].MsgID != 2914 {
		t.Errorf("pending = %+v, want [2914]", pending)
	}
}

func TestExportIncrementalFallsBackToFull(t *testing.T) {
	fake := &fakeRunner{exportJSON: deltaExportJSON}
	a := openBare(t, fake)

	// no watermark yet -> incremental must fall back to a full export
	res, err := a.Export(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if fake.lastExportOpts.SinceID != 0 {
		t.Errorf("SinceID = %d, want 0 (full)", fake.lastExportOpts.SinceID)
	}
	if res.Incremental {
		t.Error("res.Incremental = true, want false (fell back to full)")
	}
	if wm, _ := a.store.LastMsgID(); wm != 2915 {
		t.Errorf("watermark = %d, want 2915", wm)
	}
}

func TestImportRecordsContent(t *testing.T) {
	a := openBare(t, &fakeRunner{})
	path := filepath.Join(t.TempDir(), "delta.json")
	if err := os.WriteFile(path, []byte(deltaExportJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := a.Import(path); err != nil {
		t.Fatal(err)
	}

	// both the photo and the text-only message get content rows
	if n, _ := a.store.MessageCount(); n != 2 {
		t.Fatalf("MessageCount = %d, want 2", n)
	}
	photo, ok, err := a.store.ContentMessage(2914)
	if err != nil || !ok {
		t.Fatalf("content 2914 = ok %v, err %v; want present", ok, err)
	}
	if photo.Type != "message" || photo.File != "n.jpg" || photo.Text != "caption" {
		t.Errorf("content 2914 = %+v, want message/n.jpg/caption", photo)
	}
	if len(photo.Raw) == 0 {
		t.Error("content 2914 raw should be captured")
	}
	text, ok, err := a.store.ContentMessage(2915)
	if err != nil || !ok {
		t.Fatalf("content 2915 = ok %v, err %v; want present", ok, err)
	}
	if text.Text != "text only" || text.File != "" {
		t.Errorf("content 2915 = %+v, want text-only row", text)
	}
	// a manual import must never delete the file it was given
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Import removed %s: %v", path, err)
	}
}

func TestExportRemovesFileOnSuccess(t *testing.T) {
	fake := &fakeRunner{exportJSON: deltaExportJSON}
	a := openBare(t, fake)

	res, err := a.Export(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	// the DB is the archive now: the export file was transport only
	if _, err := os.Stat(res.File); !os.IsNotExist(err) {
		t.Errorf("export file %s should be gone after import, stat err = %v", res.File, err)
	}
	if n, _ := a.store.MessageCount(); n != 2 {
		t.Errorf("MessageCount = %d, want 2 (content survives the file)", n)
	}
}

func TestExportKeepsFileWhenImportFails(t *testing.T) {
	fake := &fakeRunner{exportJSON: `{"id":100,"messages":[ {"id":1,`} // truncated
	a := openBare(t, fake)

	if _, err := a.Export(context.Background(), false); err == nil {
		t.Fatal("expected an import error on invalid JSON")
	}
	if _, err := os.Stat(fake.lastExportOpts.Output); err != nil {
		t.Errorf("export file should survive a failed import: %v", err)
	}
}

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

func writeExportFile(t *testing.T, a *Archive, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(a.cfg.exportDir(), name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReplayExportsNewestSnapshotWins(t *testing.T) {
	a := openBare(t, &fakeRunner{})
	writeExportFile(t, a, "20260101-000000.json", replayDayOneJSON)
	writeExportFile(t, a, "20260102-000000.json", replayDayTwoJSON)
	// non-stamped files must be ignored even when they hold valid exports
	writeExportFile(t, a, "batch.json", replayDayTwoJSON)
	writeExportFile(t, a, "weird.txt", replayDayTwoJSON)

	// the photo was already downloaded before the migration: done, with two
	// attempts burned and a verified path recorded
	if _, err := a.store.UpsertManifest([]store.Record{{MsgID: 100, DialogID: 100, FileName: "photo.jpg", Size: 50, MediaType: "photo", Date: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.MarkAttempt(100, 0, "boom", 9); err != nil {
		t.Fatal(err)
	}
	if err := a.store.MarkAttempt(100, 0, "boom", 9); err != nil {
		t.Fatal(err)
	}
	if err := a.store.MarkDone(100, 50, "media/100_100_photo.jpg"); err != nil {
		t.Fatal(err)
	}

	files, err := a.ReplayExports()
	if err != nil {
		t.Fatal(err)
	}
	if files != 2 {
		t.Errorf("files = %d, want 2 (batch.json and weird.txt must be skipped)", files)
	}

	// the newest snapshot wins for the shared id; the text-only row lands too
	c, ok, err := a.store.ContentMessage(100)
	if err != nil || !ok {
		t.Fatalf("content 100 = ok %v, err %v; want present", ok, err)
	}
	if c.Text != "new caption" {
		t.Errorf("content 100 text = %q, want %q (day two wins)", c.Text, "new caption")
	}
	c, ok, err = a.store.ContentMessage(101)
	if err != nil || !ok {
		t.Fatalf("content 101 = ok %v, err %v; want present", ok, err)
	}
	if c.Text != "added later" || c.File != "" {
		t.Errorf("content 101 = %+v, want a text-only row added by day two", c)
	}

	// watermark advanced to the highest id seen across all snapshots
	if wm, _ := a.store.LastMsgID(); wm != 101 {
		t.Errorf("watermark = %d, want 101", wm)
	}

	// the finished download is untouched: still done, same attempts and path
	db, err := sql.Open("sqlite", filepath.Join(a.cfg.Dir, "archive.db"))
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
		if _, err := os.Stat(filepath.Join(a.cfg.exportDir(), name)); err != nil {
			t.Errorf("replay removed %s: %v", name, err)
		}
	}
}

func TestReplayExportsNothingToReplay(t *testing.T) {
	a := openBare(t, &fakeRunner{})

	// a fresh archive has an empty export/ dir: not an error, nothing to do
	files, err := a.ReplayExports()
	if err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Errorf("files = %d, want 0", files)
	}
	if n, _ := a.store.MessageCount(); n != 0 {
		t.Errorf("MessageCount = %d, want 0", n)
	}
}
