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
// for each listed id, named per dlTemplate inside tdl's own -d dir (the
// dialog's media dir). Ids absent from writeSize get no file
// (deleted/missing).
type fakeRunner struct {
	writeSize map[int]int
	calls     int
	failNext  bool // one-shot: next Download returns an infra error and writes nothing
	waitKill  bool // after writing, park until ctx is done, then still report success

	// export simulation
	exportJSON     string             // written to opts.Output on Export, if set
	lastExportOpts tdlx.ExportOptions // records the last Export call

	// chat ls simulation for refreshDialogMeta: chatList is returned verbatim,
	// or chatListErr makes the whole listing fail
	chatList         []tdlx.DialogInfo
	chatListErr      error
	lastChatListOpts tdlx.ChatListOptions
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
		name := fmt.Sprintf("%d_%s", m.ID, m.File)
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

func (f *fakeRunner) ChatList(_ context.Context, o tdlx.ChatListOptions) ([]tdlx.DialogInfo, error) {
	f.lastChatListOpts = o
	if f.chatListErr != nil {
		return nil, f.chatListErr
	}
	return f.chatList, nil
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

	seedDialog(t, a, 100, recs)
	return a
}

// seedDialog plants a dialog with content and manifest rows, the FK order the
// store demands. Retry tests use it to build multi-dialog states.
func seedDialog(t *testing.T, a *Archive, dialogID int64, recs []store.Record) {
	t.Helper()
	if err := a.store.UpsertDialog(store.Dialog{DialogID: dialogID}); err != nil {
		t.Fatal(err)
	}
	// content rows must land before the manifest: tasks reference messages
	content := make([]store.ContentRecord, len(recs))
	for i, r := range recs {
		content[i] = store.ContentRecord{MsgID: r.MsgID, Type: "message"}
	}
	if err := a.store.UpsertContent(dialogID, content); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.UpsertManifest(dialogID, recs); err != nil {
		t.Fatal(err)
	}
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
	// files land in the dialog's own media dir, named per dlTemplate
	for _, id := range []int{1, 2, 3} {
		if _, err := os.Stat(filepath.Join(a.cfg.dialogMediaDir(100), fmt.Sprintf("%d_f", id))); err != nil {
			t.Errorf("media file for msg %d: %v", id, err)
		}
	}
}

func TestDownloadStoresRelativeMediaPath(t *testing.T) {
	fake := &fakeRunner{writeSize: map[int]int{1: 1}}
	a := setup(t, Config{BatchSize: 10, MaxAttempts: 2}, fake, recsWithSizeEqualsID(1))

	if _, err := a.Download(context.Background()); err != nil {
		t.Fatal(err)
	}
	// the stored path is root-relative with slashes, not tied to where the
	// archive root lives on this machine
	db, err := sql.Open("sqlite", a.cfg.dbPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var path string
	if err := db.QueryRow(`SELECT path FROM tasks WHERE dialog_id = 100 AND msg_id = 1`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != "media/100/1_f" {
		t.Errorf("stored path = %q, want media/100/1_f", path)
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
		t.Fatalf("unknown numeric chat: resolveDialogID err = %v, want it naming 999", err)
	}

	// a username --chat matches a stored username case-insensitively, with or
	// without the '@', making sync incremental on multi-dialog roots
	if err := a.store.UpsertDialog(store.Dialog{DialogID: 100, Username: "SomeChannel"}); err != nil {
		t.Fatal(err)
	}
	for _, chat := range []string{"somechannel", "@SomeChannel"} {
		a.cfg.Chat = chat
		if id, err = a.resolveDialogID(); err != nil || id != 100 {
			t.Fatalf("username chat %q: resolveDialogID = %d,%v; want 100,nil", chat, id, err)
		}
	}

	// an unmatched username is actionable like an unmatched id, never a
	// fall-through to the sole dialog
	a.cfg.Chat = "nobody"
	if _, err = a.resolveDialogID(); err == nil || !strings.Contains(err.Error(), "nobody") {
		t.Fatalf("unknown username chat: resolveDialogID err = %v, want it naming nobody", err)
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
		t.Errorf("ambiguous resolveDialogID err = %v, want it listing 100, 200", err)
	}

	// an empty state names the export remedy
	a2 := openBare(t, fake)
	if _, err = a2.resolveDialogID(); err == nil || !strings.Contains(err.Error(), "run export first") {
		t.Errorf("empty resolveDialogID err = %v, want it pointing at export", err)
	}
}

// TestResolveChatDialogFlagScope checks the status/reset-failed filter entry:
// no --chat means "every dialog", a set --chat resolves or errors, and the
// last-operation cache must not leak in.
func TestResolveChatDialogFlagScope(t *testing.T) {
	fake := &fakeRunner{}
	a := setup(t, Config{}, fake, nil)

	if id, filtered, err := a.ResolveChatDialog(); filtered || err != nil || id != 0 {
		t.Fatalf("no chat: ResolveChatDialog = %d,%v,%v; want 0,false,nil", id, filtered, err)
	}
	if err := a.store.UpsertDialog(store.Dialog{DialogID: 100, Username: "chan"}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.UpsertDialog(store.Dialog{DialogID: 200}); err != nil {
		t.Fatal(err)
	}
	a.cfg.Chat = "@Chan"
	id, filtered, err := a.ResolveChatDialog()
	if err != nil || !filtered || id != 100 {
		t.Fatalf("username chat: ResolveChatDialog = %d,%v,%v; want 100,true,nil", id, filtered, err)
	}
	// even with an import having pinned a dialog, the flag resolves on its own
	a.dialogID = 200
	if id, _, err = a.ResolveChatDialog(); err != nil || id != 100 {
		t.Fatalf("cached dialog must not override the flag: ResolveChatDialog = %d,%v", id, err)
	}
	a.cfg.Chat = "300"
	if _, _, err = a.ResolveChatDialog(); err == nil || !strings.Contains(err.Error(), "300") {
		t.Fatalf("unknown chat: ResolveChatDialog err = %v, want it naming 300", err)
	}
}

func TestNormalizeChat(t *testing.T) {
	cases := map[string]string{
		// Bot-API marked channel form: -(1e12 + bare)
		"-1003992083278": "3992083278",
		// a negative id that is NOT a marked channel keeps only the sign dropped
		"-12345":         "12345",
		"3992083278":     "3992083278",
		"1003992083278":  "3992083278",
		"@foo":           "@foo",
		"foo":            "foo",
		"https://t.me/x": "https://t.me/x",
		"":               "",
	}
	for in, want := range cases {
		if got := normalizeChat(in); got != want {
			t.Errorf("normalizeChat(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOpenNormalizesChat proves the canonicalization rides into cfg.Chat for
// every later consumer (export, dialog resolution, filters).
func TestOpenNormalizesChat(t *testing.T) {
	a, err := Open(Config{Dir: t.TempDir(), Chat: "-1003992083278"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if a.Chat() != "3992083278" {
		t.Errorf("Chat = %q, want 3992083278", a.Chat())
	}
}

func TestOpenRejectsLegacyLayout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, legacyDBName), []byte("v2 db"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Open(Config{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "one-channel-per-directory") || !strings.Contains(err.Error(), "migrate db") {
		t.Fatalf("Open err = %v, want it naming the legacy layout and the migrate db remedy", err)
	}

	// once the new DB exists the guard stands down, whatever else sits nearby
	if err := os.WriteFile(filepath.Join(dir, dbName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Open with both DBs present: %v", err)
	}
	_ = a.Close()
}

// blockingRunner stands in for a hung tdl: Download blocks until its context
// is done, first appending to a .tmp file every 50ms for growFor to simulate
// a live transfer that then goes idle.
type blockingRunner struct {
	growFor time.Duration
}

func (b *blockingRunner) Check(context.Context) error { return nil }

func (b *blockingRunner) Export(context.Context, tdlx.ExportOptions) error { return nil }

func (b *blockingRunner) ChatList(context.Context, tdlx.ChatListOptions) ([]tdlx.DialogInfo, error) {
	return nil, nil
}

func (b *blockingRunner) Download(ctx context.Context, o tdlx.DownloadOptions) error {
	if b.growFor > 0 {
		part := filepath.Join(o.Dir, "1_live.bin.tmp")
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

// TestIdleWatchdogScopesToDialog pins the watchdog to the downloaded dialog's
// own media dir: another dialog's downloads must not keep a stalled batch alive.
func TestIdleWatchdogScopesToDialog(t *testing.T) {
	a := setup(t,
		Config{BatchSize: 10, MaxAttempts: 3, IdleTimeout: 300 * time.Millisecond},
		&blockingRunner{}, recsWithSizeEqualsID(1))
	a.idleSample = 50 * time.Millisecond
	// a second dialog exists, so pin which one Download serves
	a.cfg.Chat = "100"
	seedDialog(t, a, 200, nil)

	// churn in ANOTHER dialog's media dir for the whole stall window
	stop := make(chan struct{})
	go func() {
		dir := a.cfg.dialogMediaDir(200)
		_ = os.MkdirAll(dir, 0o755)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("noise_%d", i)), make([]byte, 1024), 0o644)
			time.Sleep(50 * time.Millisecond)
		}
	}()
	defer close(stop) // the goroutine exits on its next 50ms tick

	start := time.Now()
	_, err := a.Download(context.Background())
	elapsed := time.Since(start)
	if err == nil || !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want an idle kill despite other-dialog activity", err)
	}
	// the foreign churn must buy the stalled batch no time beyond the idle
	// window plus a sampling grace period
	if elapsed > 700*time.Millisecond {
		t.Errorf("killed after %s, want ~the 300ms window: other-dialog activity must not stall the watchdog", elapsed)
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
	write("1_a.jpg", 10)
	write("2_b.jpg.tmp", 20) // in-progress files count toward growth
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

func TestRetryTasksRetriesFailedTask(t *testing.T) {
	fake := &fakeRunner{writeSize: map[int]int{1: 999, 2: 2}}
	a := setup(t, Config{BatchSize: 10, MaxAttempts: 1}, fake, recsWithSizeEqualsID(1, 2))

	// first run: msg 1 lands at the wrong size and, at one allowed attempt,
	// fails; msg 2 is done
	res, err := a.Download(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Done != 1 || res.Failed != 1 {
		t.Fatalf("first run res = %+v, want Done=1 Failed=1", res)
	}

	// whatever failed is fixed; retry exactly that task
	fake.writeSize[1] = 1
	rres, err := a.RetryTasks(context.Background(), []TaskRef{{DialogID: 100, MsgID: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if rres.Done != 1 || rres.Failed != 0 || rres.Passes != 1 {
		t.Errorf("retry res = %+v, want Done=1 Failed=0 Passes=1", rres)
	}
	if pending, _ := a.store.ListPending(100); len(pending) != 0 {
		t.Errorf("pending after retry = %d, want 0", len(pending))
	}
	if failed, _ := a.store.ListFailed(100); len(failed) != 0 {
		t.Errorf("failed after retry = %d, want 0", len(failed))
	}
}

// TestRetryTasksDoneTask checks the regardless-of-prior-status promise on a
// DONE task: it flips back to pending and re-verifies. The fake writes nothing
// on the retry, and verify still passes on the intact file — the in-the-large
// version of tdl's --skip-same keeping a re-download cheap.
func TestRetryTasksDoneTask(t *testing.T) {
	fake := &fakeRunner{writeSize: map[int]int{1: 1}}
	a := setup(t, Config{BatchSize: 10, MaxAttempts: 2}, fake, recsWithSizeEqualsID(1))

	if _, err := a.Download(context.Background()); err != nil {
		t.Fatal(err)
	}

	delete(fake.writeSize, 1) // tdl "skips" the intact file this time
	res, err := a.RetryTasks(context.Background(), []TaskRef{{DialogID: 100, MsgID: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Done != 1 || res.Failed != 0 {
		t.Errorf("retry res = %+v, want Done=1 Failed=0 (intact file re-verifies)", res)
	}
	counts, _ := a.store.CountsAll()
	if counts[store.StatusDone] != 1 || counts[store.StatusPending] != 0 {
		t.Errorf("counts = %v, want the task still done and nothing pending", counts)
	}
}

func TestRetryTasksMissingTaskErrors(t *testing.T) {
	a := setup(t, Config{}, &fakeRunner{}, recsWithSizeEqualsID(1))

	_, err := a.RetryTasks(context.Background(), []TaskRef{{DialogID: 100, MsgID: 55}})
	if err == nil || !strings.Contains(err.Error(), "100/55") {
		t.Fatalf("retry of a missing task: err = %v, want it naming 100/55", err)
	}

	// an unknown dialog is the same actionable error, not a silent no-op
	if _, err = a.RetryTasks(context.Background(), []TaskRef{{DialogID: 999, MsgID: 1}}); err == nil || !strings.Contains(err.Error(), "999/1") {
		t.Fatalf("retry of an unknown dialog: err = %v, want it naming 999/1", err)
	}
}

func TestRetryTasksGroupsByDialog(t *testing.T) {
	fake := &fakeRunner{writeSize: map[int]int{1: 1, 2: 2}}
	a := setup(t, Config{BatchSize: 10, MaxAttempts: 2}, fake, recsWithSizeEqualsID(1))
	seedDialog(t, a, 200, recsWithSizeEqualsID(2))

	// push one task of each dialog to failed without a full download (a
	// Download cannot even resolve which dialog to serve here)
	if err := a.store.MarkAttempt(100, 1, 0, "simulated", 1); err != nil {
		t.Fatal(err)
	}
	if err := a.store.MarkAttempt(200, 2, 0, "simulated", 1); err != nil {
		t.Fatal(err)
	}

	res, err := a.RetryTasks(context.Background(), []TaskRef{
		{DialogID: 200, MsgID: 2},
		{DialogID: 100, MsgID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Done != 2 || res.Failed != 0 || res.Passes != 2 {
		t.Errorf("retry res = %+v, want Done=2 Failed=0 Passes=2 (one per dialog)", res)
	}
	// each dialog's file landed in its own media dir
	for _, c := range []struct {
		dialogID int64
		name     string
	}{{100, "1_f"}, {200, "2_f"}} {
		if _, err := os.Stat(filepath.Join(a.cfg.dialogMediaDir(c.dialogID), c.name)); err != nil {
			t.Errorf("media %d/%s: %v", c.dialogID, c.name, err)
		}
	}
}

// TestRetryTasksExcludesUnretriedFailures pins the Failed counter to the
// retried refs: unrelated failures must not surface in the retry report.
func TestRetryTasksExcludesUnretriedFailures(t *testing.T) {
	fake := &fakeRunner{writeSize: map[int]int{2: 2}}
	a := setup(t, Config{BatchSize: 10, MaxAttempts: 1}, fake, recsWithSizeEqualsID(1, 2, 3))
	// msgs 2 and 3 fail; only 2 is retried
	if err := a.store.MarkAttempt(100, 2, 0, "simulated", 1); err != nil {
		t.Fatal(err)
	}
	if err := a.store.MarkAttempt(100, 3, 0, "simulated", 1); err != nil {
		t.Fatal(err)
	}

	res, err := a.RetryTasks(context.Background(), []TaskRef{{DialogID: 100, MsgID: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Done != 1 || res.Failed != 0 {
		t.Errorf("retry res = %+v, want Done=1 Failed=0 (msg 3 stays failed but unretried)", res)
	}
	if failed, _ := a.store.ListFailed(100); len(failed) != 1 || failed[0].MsgID != 3 {
		t.Errorf("failed = %+v, want only msg 3", failed)
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
	// the resolvable dialog's export file lands under its own folder
	if want := a.cfg.dialogExportDir(100); filepath.Dir(res.File) != want {
		t.Errorf("export file dir = %s, want %s", filepath.Dir(res.File), want)
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
	// an unresolvable dialog puts the transient file under the root export/
	if want := a.cfg.exportDir(); filepath.Dir(res.File) != want {
		t.Errorf("export file dir = %s, want %s", filepath.Dir(res.File), want)
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

func TestRefreshDialogMetaUpserts(t *testing.T) {
	fake := &fakeRunner{chatList: []tdlx.DialogInfo{{
		ID: 100, Type: "channel", Title: "Some Channel", Username: "somechannel",
	}}}
	a := openBare(t, fake)
	if err := a.store.UpsertDialog(store.Dialog{DialogID: 100}); err != nil {
		t.Fatal(err)
	}

	a.refreshDialogMeta(context.Background(), 100)

	// the lookup fetches exactly this dialog, not the whole dialog list
	if fake.lastChatListOpts.Filter != "ID == 100" {
		t.Errorf("chat ls filter = %q, want ID == 100", fake.lastChatListOpts.Filter)
	}
	d, ok, err := a.store.GetDialog(100)
	if err != nil || !ok {
		t.Fatalf("GetDialog = %v,%v; want the dialog", ok, err)
	}
	if d.Username != "somechannel" || d.Title != "Some Channel" || d.Kind != "channel" {
		t.Errorf("dialog = %+v, want the chat ls username/title/kind", d)
	}
	if d.Namespace != "default" {
		t.Errorf("namespace = %q, want the archive's default", d.Namespace)
	}
}

func TestRefreshDialogMetaBestEffort(t *testing.T) {
	fake := &fakeRunner{chatListErr: fmt.Errorf("chat ls exploded")}
	a := openBare(t, fake)
	if err := a.store.UpsertDialog(store.Dialog{DialogID: 100}); err != nil {
		t.Fatal(err)
	}

	// a failed chat ls (and, likewise, a zero- or multi-hit list) only warns;
	// the export itself already succeeded
	a.refreshDialogMeta(context.Background(), 100)
	fake.chatListErr = nil
	fake.chatList = []tdlx.DialogInfo{{ID: 100}, {ID: 100}}
	a.refreshDialogMeta(context.Background(), 100)

	d, ok, err := a.store.GetDialog(100)
	if err != nil || !ok {
		t.Fatalf("GetDialog = %v,%v; want the dialog", ok, err)
	}
	if d.Username != "" || d.Title != "" || d.Kind != "" {
		t.Errorf("dialog = %+v, want no metadata from failed lookups", d)
	}
}

// TestExportRefreshesDialogMeta wires the refresh into Export: after a
// successful import, the exported dialog carries chat ls metadata.
func TestExportRefreshesDialogMeta(t *testing.T) {
	fake := &fakeRunner{
		exportJSON: deltaExportJSON,
		chatList:   []tdlx.DialogInfo{{ID: 100, Type: "channel", Title: "Some Channel", Username: "somechannel"}},
	}
	a := openBare(t, fake)

	if _, err := a.Export(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	d, ok, err := a.store.GetDialog(100)
	if err != nil || !ok {
		t.Fatalf("GetDialog = %v,%v; want the dialog", ok, err)
	}
	if d.Username != "somechannel" || d.Title != "Some Channel" || d.Kind != "channel" {
		t.Errorf("dialog = %+v, want chat ls metadata after export", d)
	}
}
