package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myl7/tgxiv/internal/store"
	"github.com/myl7/tgxiv/internal/tdlx"
)

// fakeRunner stands in for the tdl binary. On Download it reads the batch file
// (proving writeBatch is well-formed) and creates a file of the configured size
// for each listed id. Ids absent from writeSize get no file (deleted/missing).
type fakeRunner struct {
	writeSize map[int]int
	calls     int
	failNext  bool // one-shot: next Download returns an infra error and writes nothing
	waitKill  bool // after writing, park until ctx is done, then still report success

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

func (f *fakeRunner) Download(ctx context.Context, o tdlx.DownloadOptions) error {
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
	if f.waitKill {
		// every file is on disk; park until the watchdog kills the batch and
		// report success anyway, to hit the fired-idle-but-tdl-won race
		<-ctx.Done()
		return nil
	}
	return nil
}

func setup(t *testing.T, cfg Config, runner tdlRunner, recs []store.Record) *Archive {
	t.Helper()
	cfg.Dir = t.TempDir()
	a, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	a.runner = runner

	if err := a.store.UpsertDialog(store.Dialog{DialogID: 100}); err != nil {
		t.Fatal(err)
	}
	// content rows must land before the manifest: tasks reference messages
	content := make([]store.ContentRecord, len(recs))
	for i, r := range recs {
		content[i] = store.ContentRecord{MsgID: r.MsgID, Type: "message"}
	}
	if err := a.store.UpsertContent(100, content); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.UpsertManifest(100, recs); err != nil {
		t.Fatal(err)
	}
	return a
}

func recsWithSizeEqualsID(ids ...int) []store.Record {
	var out []store.Record
	for _, id := range ids {
		out = append(out, store.Record{MsgID: id, Size: int64(id), FileName: "f"})
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
	counts, _ := a.store.CountsAll()
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
	failed, _ := a.store.ListFailed(100)
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
	pending, _ := a.store.ListPending(100)
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	counts, _ := a.store.CountsAll()
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

func TestResolveDialogID(t *testing.T) {
	fake := &fakeRunner{}
	a := setup(t, Config{}, fake, nil) // seeds dialog 100

	// a lone dialog is unambiguous
	id, err := a.resolveDialogID()
	if err != nil || id != 100 {
		t.Fatalf("resolveDialogID = %d,%v; want 100,nil", id, err)
	}

	// a numeric --chat addressing a known dialog wins
	a.cfg.Chat = "100"
	if id, err = a.resolveDialogID(); err != nil || id != 100 {
		t.Fatalf("numeric chat: resolveDialogID = %d,%v; want 100,nil", id, err)
	}

	// a numeric --chat naming an UNKNOWN dialog is an error even though the
	// sole dialog could have absorbed it: resolving to the wrong dialog would
	// sync against the wrong watermark and skip real messages
	a.cfg.Chat = "999"
	if _, err = a.resolveDialogID(); err == nil || !strings.Contains(err.Error(), "999") {
		t.Fatalf("unknown numeric chat: resolveDialogID err = %v; want it naming 999", err)
	}
	a.cfg.Chat = ""

	// the cached id of the last import wins even with --chat set elsewhere
	a.dialogID = 7
	if id, err = a.resolveDialogID(); err != nil || id != 7 {
		t.Fatalf("cached id: resolveDialogID = %d,%v; want 7,nil", id, err)
	}

	// two dialogs and no way to tell them apart is an error naming both
	a.dialogID = 0
	a.cfg.Chat = ""
	if err := a.store.UpsertDialog(store.Dialog{DialogID: 200}); err != nil {
		t.Fatal(err)
	}
	_, err = a.resolveDialogID()
	if err == nil || !strings.Contains(err.Error(), "100, 200") {
		t.Errorf("ambiguous resolveDialogID err = %v; want it listing 100, 200", err)
	}

	// an empty state names the export remedy
	a2 := openBare(t, fake)
	if _, err = a2.resolveDialogID(); err == nil || !strings.Contains(err.Error(), "run export first") {
		t.Errorf("empty resolveDialogID err = %v; want it pointing at export", err)
	}
}

// blockingRunner stands in for a hung tdl: Download blocks until its context
// is done, first appending to a .tmp file every 50ms for growFor to simulate
// a live transfer that then goes idle.
type blockingRunner struct {
	growFor time.Duration
}

func (b *blockingRunner) Check(context.Context) error { return nil }

func (b *blockingRunner) Export(context.Context, tdlx.ExportOptions) error { return nil }

func (b *blockingRunner) Download(ctx context.Context, o tdlx.DownloadOptions) error {
	if b.growFor > 0 {
		part := filepath.Join(o.Dir, "100_1_live.bin.tmp")
		deadline := time.Now().Add(b.growFor)
		for time.Now().Before(deadline) {
			f, err := os.OpenFile(part, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			_, _ = f.Write(make([]byte, 2048))
			_ = f.Close()
			select {
			case <-time.After(50 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestDownloadIdleTimeoutBurnsAttempt(t *testing.T) {
	a := setup(t,
		Config{BatchSize: 10, MaxAttempts: 3, IdleTimeout: 200 * time.Millisecond},
		&blockingRunner{}, recsWithSizeEqualsID(1))
	a.idleSample = 50 * time.Millisecond

	_, err := a.Download(context.Background())
	if err == nil || !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want an error wrapping ErrIdleTimeout", err)
	}
	// the stalled message burns one attempt but stays pending
	pending, _ := a.store.ListPending(100)
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	if n, ok, _ := a.store.Attempts(100, 1); !ok || n != 1 {
		t.Errorf("attempts = %d (ok %v), want 1", n, ok)
	}
}

func TestIdleWatchdogGrowthPreventsKill(t *testing.T) {
	a := setup(t,
		Config{BatchSize: 10, MaxAttempts: 3, IdleTimeout: 300 * time.Millisecond},
		&blockingRunner{growFor: 400 * time.Millisecond}, recsWithSizeEqualsID(1))
	a.idleSample = 50 * time.Millisecond

	start := time.Now()
	_, err := a.Download(context.Background())
	elapsed := time.Since(start)
	if err == nil || !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want an error wrapping ErrIdleTimeout", err)
	}
	// growth keeps resetting the idle clock, so the kill must postdate the
	// 400ms of live transfer plus the 300ms idle window, not just 300ms
	if elapsed < 400*time.Millisecond {
		t.Errorf("killed after %s, want >= the 400ms of live transfer", elapsed)
	}
}

func TestIdleWatchdogUserCancelNotIdle(t *testing.T) {
	a := setup(t,
		Config{BatchSize: 10, MaxAttempts: 3, IdleTimeout: 1 * time.Second},
		&blockingRunner{}, recsWithSizeEqualsID(1))
	a.idleSample = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := a.Download(ctx)
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrIdleTimeout) {
			t.Fatalf("err = %v, want context.Canceled and not an idle kill", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Download did not return after cancel")
	}
	// a user stop must not burn an attempt
	if n, ok, _ := a.store.Attempts(100, 1); !ok || n != 0 {
		t.Errorf("attempts = %d (ok %v), want 0", n, ok)
	}
}

func TestRunBatchIdleFiresAfterRunnerSuccess(t *testing.T) {
	// waitKill makes the runner write every file, get killed by the watchdog,
	// and still report success: the idle flag is set, but the batch won
	fake := &fakeRunner{writeSize: map[int]int{1: 1}, waitKill: true}
	a := setup(t,
		Config{BatchSize: 10, MaxAttempts: 3, IdleTimeout: 100 * time.Millisecond},
		fake, recsWithSizeEqualsID(1))
	a.idleSample = 20 * time.Millisecond

	if err := a.runBatch(context.Background(), 100, recsWithSizeEqualsID(1)); err != nil {
		t.Fatalf("runBatch err = %v, want nil (success beats the late idle kill)", err)
	}
}

func TestDirBytes(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, n int) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("100_1_a.jpg", 10)
	write("100_2_b.jpg.tmp", 20) // in-progress files count toward growth
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join("sub", "c.bin"), 5)

	got, err := dirBytes(dir)
	if err != nil || got != 35 {
		t.Errorf("dirBytes = %d, err %v; want 35, nil", got, err)
	}
	got, err = dirBytes(filepath.Join(dir, "missing"))
	if err != nil || got != 0 {
		t.Errorf("dirBytes(missing) = %d, err %v; want 0, nil", got, err)
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

	// pretend a previous sync of dialog 100 reached message 2913
	if err := a.store.UpsertDialog(store.Dialog{DialogID: 100}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.AdvanceLastMsgID(100, 2913); err != nil {
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
	if wm, _ := a.store.LastMsgID(100); wm != 2915 {
		t.Errorf("watermark = %d, want 2915", wm)
	}
	// the new photo is now pending
	pending, _ := a.store.ListPending(100)
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
	if wm, _ := a.store.LastMsgID(100); wm != 2915 {
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
	// the import also created the dialog row for the export's channel id
	if a.DialogID() != 100 {
		t.Errorf("DialogID = %d, want 100", a.DialogID())
	}

	// both the photo and the text-only message get content rows
	if n, _ := a.store.MessageCount(100); n != 2 {
		t.Fatalf("MessageCount = %d, want 2", n)
	}
	db, err := sql.Open("sqlite", a.cfg.dbPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	contentRow := func(msgID int) (typ, file, text, raw string) {
		t.Helper()
		if err := db.QueryRow(
			`SELECT type, file, text, raw FROM messages WHERE dialog_id = 100 AND msg_id = ?`, msgID,
		).Scan(&typ, &file, &text, &raw); err != nil {
			t.Fatalf("content %d: %v", msgID, err)
		}
		return
	}
	typ, file, text, raw := contentRow(2914)
	if typ != "message" || file != "n.jpg" || text != "caption" {
		t.Errorf("content 2914 = %q/%q/%q, want message/n.jpg/caption", typ, file, text)
	}
	if len(raw) == 0 {
		t.Error("content 2914 raw should be captured")
	}
	typ, file, text, _ = contentRow(2915)
	if text != "text only" || file != "" {
		t.Errorf("content 2915 = %q/%q, want a text-only row", text, file)
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
	if n, _ := a.store.MessageCount(100); n != 2 {
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
