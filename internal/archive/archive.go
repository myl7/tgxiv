// Package archive orchestrates a dialog archive: export the manifest with tdl,
// record it in a state DB, then download media smallest-first in batches,
// verifying each file by size and retrying a bounded number of times.
package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/myl7/tgxiv/internal/exportjson"
	"github.com/myl7/tgxiv/internal/media"
	"github.com/myl7/tgxiv/internal/store"
	"github.com/myl7/tgxiv/internal/tdlx"
)

// ErrIdleTimeout marks a tdl batch killed by the idle watchdog (runBatch).
var ErrIdleTimeout = errors.New("tdl idle timeout")

// Disk layout under the archive root, one folder per dialog so a root holds
// many dialogs without filename collisions: media/<dialogID>/ carries the
// downloaded files, export/<dialogID>/ the transient export and batch files,
// and logs/ stays flat at the root. dbName/legacyDBName guard against opening
// a directory that still uses the pre-multi-dialog layout (see Open).
const (
	dbName        = "tgxiv.sqlite"
	legacyDBName  = "archive.db"
	mediaDirName  = "media"
	exportDirName = "export"
	logsDirName   = "logs"

	// dlTemplate names tdl's output files "<msgID>_<file name>" inside the
	// dialog's media dir. The dialog id moved from the old filename prefix
	// ("<dialogID>_<msgID>_<name>", tdl's default) into the folder name, so
	// files stay unique per dialog without the redundant prefix.
	dlTemplate = "{{ .MessageID }}_{{ filenamify .FileName }}"
)

// Config is the archive's static configuration.
type Config struct {
	Dir         string        // archive root directory
	Chat        string        // channel username, id, or link (for export); normalized by Open
	Namespace   string        // tdl session namespace
	TdlBin      string        // tdl executable
	BatchSize   int           // messages per tdl dl invocation
	MaxAttempts int           // per-message download attempts before giving up
	Threads     int           // tdl --threads (0 = tdl default)
	Limit       int           // tdl --limit concurrent files (0 = tdl default)
	IdleTimeout time.Duration // kill a batch whose media dir stopped growing for this long (0 = off)
}

// tdlRunner is the slice of tdlx.Runner the archive depends on. It is an
// interface so tests can drive the orchestration without a real tdl binary.
type tdlRunner interface {
	Check(context.Context) error
	Export(context.Context, tdlx.ExportOptions) error
	Download(context.Context, tdlx.DownloadOptions) error
	ChatList(context.Context, tdlx.ChatListOptions) ([]tdlx.DialogInfo, error)
}

// Archive binds a Config to its open state DB and tdl runner. The DB holds
// every dialog of the archive root, so the dialog a Download operates on is
// resolved on demand (resolveDialogID).
type Archive struct {
	cfg    Config
	store  *store.Store
	runner tdlRunner
	// dialogID caches the dialog the last Import or Download operated on, so
	// a following step (failed-report, next Download) need not re-resolve it.
	dialogID int64
	// idleSample is the idle watchdog's dir-size sampling interval, derived
	// from IdleTimeout in Open; tests shrink it to keep runs fast.
	idleSample time.Duration
}

// Layout helpers for the standard sub-paths under the archive dir.
func (c Config) dbPath() string    { return filepath.Join(c.Dir, dbName) }
func (c Config) mediaDir() string  { return filepath.Join(c.Dir, mediaDirName) }
func (c Config) exportDir() string { return filepath.Join(c.Dir, exportDirName) }
func (c Config) logsDir() string   { return filepath.Join(c.Dir, logsDirName) }

// dialogMediaDir is where tdl downloads the dialog's files: media/<dialogID>/.
func (c Config) dialogMediaDir(dialogID int64) string {
	return filepath.Join(c.mediaDir(), strconv.FormatInt(dialogID, 10))
}

// dialogExportDir holds the dialog's transient export JSON and batch files:
// export/<dialogID>/.
func (c Config) dialogExportDir(dialogID int64) string {
	return filepath.Join(c.exportDir(), strconv.FormatInt(dialogID, 10))
}

// Open prepares the archive directory, opens the DB, and builds the tdl runner.
func Open(cfg Config) (*Archive, error) {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.TdlBin == "" {
		cfg.TdlBin = "tdl"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	// canonicalize the numeric forms of --chat up front, so every consumer
	// (export, dialog resolution, filters) speaks the bare positive id
	cfg.Chat = normalizeChat(cfg.Chat)

	// Legacy-layout guard: a v2-era root carries archive.db and no tgxiv.sqlite.
	// Silently adopting such a directory would build a second, parallel DB next
	// to the old one and re-download everything into the new layout, so fail
	// loudly and point at the converter instead.
	if _, err := os.Stat(cfg.dbPath()); os.IsNotExist(err) {
		if _, err := os.Stat(filepath.Join(cfg.Dir, legacyDBName)); err == nil {
			return nil, fmt.Errorf("%s uses the old one-channel-per-directory layout that tgxiv no longer supports", cfg.Dir)
		}
	}

	// Per-dialog folders under media/ and export/ appear on demand (MkdirAll
	// in Export/runBatch); the roots themselves are created here. export/'s
	// root included: the transient export scratch space expects it to
	// exist without a prior tgxiv write.
	for _, d := range []string{cfg.Dir, cfg.mediaDir(), cfg.exportDir(), cfg.logsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}

	st, err := store.Open(cfg.dbPath())
	if err != nil {
		return nil, err
	}

	a := &Archive{
		cfg:   cfg,
		store: st,
		runner: &tdlx.Runner{
			Bin:       cfg.TdlBin,
			Namespace: cfg.Namespace,
		},
	}
	if cfg.IdleTimeout > 0 {
		// sample at a quarter of the timeout, bounded so tiny timeouts stay
		// cheap to walk and huge ones still react within 30s
		a.idleSample = cfg.IdleTimeout / 4
		if a.idleSample < 50*time.Millisecond {
			a.idleSample = 50 * time.Millisecond
		}
		if a.idleSample > 30*time.Second {
			a.idleSample = 30 * time.Second
		}
	}
	return a, nil
}

// Close releases the state DB.
func (a *Archive) Close() error { return a.store.Close() }

// Store exposes the underlying store for read-only reporting (status command).
func (a *Archive) Store() *store.Store { return a.store }

// Chat returns the configured channel identifier, in its normalized form.
func (a *Archive) Chat() string { return a.cfg.Chat }

// DialogID returns the dialog the last Import or Download operated on, or 0
// when neither has run for this Archive.
func (a *Archive) DialogID() int64 { return a.dialogID }

// LogsDir returns the archive's logs directory.
func (a *Archive) LogsDir() string { return a.cfg.logsDir() }

// normalizeChat canonicalizes a --chat value for the numeric forms Telegram
// tools emit. Telegram's Bot API "marks" channel ids as -(1e12 + bare) and
// clients often quote the marked form, while tdl and the state DB speak the
// bare positive id: a purely numeric chat has its leading '-' dropped and a
// 100-prefixed id above 1e12 unmarked. Anything non-numeric — a username or a
// t.me link — passes through untouched.
func normalizeChat(s string) string {
	digits := strings.TrimPrefix(s, "-")
	if digits == "" {
		return s
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return s // usernames and links are not ids to rewrite
	}
	if n > 1e12 {
		if bare, ok := strings.CutPrefix(digits, "100"); ok {
			digits = bare
		}
	}
	return digits
}

// ExportResult summarizes an export+import.
type ExportResult struct {
	File        string // export JSON path; transient, removed once imported
	Added       int    // new media messages added to the manifest
	SinceID     int    // id floor passed to tdl; 0 means a full export
	Incremental bool   // an incremental delta was actually applied
}

// Export runs a tdl export into the export dir and imports it into the DB.
// The export file itself is transient: it is deleted once imported. When
// incremental is true it fetches only messages at or above the resolved
// dialog's watermark; with no watermark — or no resolvable dialog — yet it
// falls back to a full export.
func (a *Archive) Export(ctx context.Context, incremental bool) (ExportResult, error) {
	if err := a.runner.Check(ctx); err != nil {
		return ExportResult{}, err
	}

	// Resolve once up front: the incremental watermark needs the dialog, and
	// so does the transient export file's per-dialog placement. Unresolvable —
	// a first export, or a multi-dialog root with no --chat — simply means a
	// full export whose file lands under the root export/ dir.
	dialogID, _ := a.resolveDialogID()

	sinceID := 0
	if incremental && dialogID != 0 {
		wm, err := a.store.LastMsgID(dialogID)
		if err != nil {
			return ExportResult{}, err
		}
		if wm > 0 {
			sinceID = wm + 1 // strictly newer than what we have
		}
	}

	stamp := time.Now().Format("20060102-150405")
	out := filepath.Join(a.cfg.exportDir(), stamp+".json")
	if dialogID != 0 {
		out = filepath.Join(a.cfg.dialogExportDir(dialogID), stamp+".json")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return ExportResult{}, fmt.Errorf("create %s: %w", filepath.Dir(out), err)
	}

	if err := a.runner.Export(ctx, tdlx.ExportOptions{
		Chat:    a.cfg.Chat,
		Output:  out,
		SinceID: sinceID,
	}); err != nil {
		return ExportResult{}, fmt.Errorf("tdl export: %w", err)
	}

	added, err := a.Import(out)
	if err != nil {
		return ExportResult{}, err
	}
	// the export file is transport, not the archive: the DB now holds its
	// content, so drop it (best effort, like the batch files)
	_ = os.Remove(out)
	// with the dialog id now learned from the export, refresh its metadata
	// while the tdl session is live (best-effort; see refreshDialogMeta)
	a.refreshDialogMeta(ctx, a.dialogID)
	return ExportResult{
		File:        out,
		Added:       added,
		SinceID:     sinceID,
		Incremental: incremental && sinceID > 0,
	}, nil
}

// Import parses a tdl export JSON and records it in the DB: the dialog row is
// created (or refreshed) first because messages and tasks reference it, then
// every message gets a content row (text-only and service messages included;
// the DB is the text archive), the media subset is additionally upserted into
// the download manifest, and finally the watermark advances.
func (a *Archive) Import(path string) (added int, err error) {
	var contentRecs []store.ContentRecord
	var recs []store.Record
	maxSeen := 0 // highest id of ANY message, for the incremental watermark
	dialogID, err := exportjson.ParseFile(path, func(m exportjson.Message) error {
		if m.ID > maxSeen {
			maxSeen = m.ID
		}
		contentRecs = append(contentRecs, store.ContentRecord{
			MsgID: m.ID,
			Type:  m.Type,
			Date:  m.Date,
			Text:  m.Text,
			File:  m.File,
			Raw:   string(m.Raw),
		})
		info, ok := media.Extract(m.Raw)
		if !ok {
			return nil
		}
		name := m.File
		if name == "" {
			name = info.Name
		}
		recs = append(recs, store.Record{
			MsgID:     m.ID,
			FileName:  name,
			Size:      info.Size,
			MediaType: string(info.Type),
		})
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("parse export: %w", err)
	}

	// dialogs -> messages -> tasks is the FK order: the dialog row must land
	// before the content rows that reference it
	if err := a.store.UpsertDialog(store.Dialog{DialogID: dialogID}); err != nil {
		return 0, err
	}
	a.writeDialogMarker(dialogID)
	if err := a.store.UpsertContent(dialogID, contentRecs); err != nil {
		return 0, err
	}
	added, err = a.store.UpsertManifest(dialogID, recs)
	if err != nil {
		return 0, err
	}
	// advance the watermark last: only after content and manifest are committed
	if err := a.store.AdvanceLastMsgID(dialogID, maxSeen); err != nil {
		return added, err
	}
	a.dialogID = dialogID
	return added, nil
}

// refreshDialogMeta backfills the dialog's username, title, and kind from a
// live "tdl chat ls" filtered to the id: export JSONs carry none of those,
// but status output and username --chat matching want them. It is strictly
// best-effort — the export has already succeeded, so an unavailable list or a
// failed upsert only warns and the next export refreshes again.
func (a *Archive) refreshDialogMeta(ctx context.Context, dialogID int64) {
	if dialogID == 0 {
		return
	}
	ds, err := a.runner.ChatList(ctx, tdlx.ChatListOptions{Filter: fmt.Sprintf("ID == %d", dialogID)})
	if err != nil {
		fmt.Printf("[archive] dialog metadata unavailable: %v\n", err)
		return
	}
	if len(ds) != 1 {
		fmt.Printf("[archive] dialog metadata unavailable: chat ls returned %d dialogs for id %d\n", len(ds), dialogID)
		return
	}
	d := ds[0]
	if err := a.store.UpsertDialog(store.Dialog{
		DialogID: dialogID,
		Username: d.Username,
		Title:    d.Title,
		Kind:     d.Type,
	}); err != nil {
		fmt.Printf("[archive] dialog metadata unavailable: %v\n", err)
		return
	}
	// the stored row changed: refresh the marker from it
	a.writeDialogMarker(dialogID)
}

// markerName is the marker file WriteDialogMarker drops into each dialog's
// media folder; see WriteDialogMarker for why the name can never collide
// with a downloaded file.
const markerName = "dialog.txt"

// WriteDialogMarker drops a short human-readable key-value pointer into the
// dialog's media folder so a person browsing media/ can tell which channel
// the numeric folder belongs to: "dialog_id:" always, "title:"/"username:"
// lines only when the row knows them (username in its stored bare, no-'@'
// form). The name dialog.txt can never collide with a downloaded file: every
// tdl download is named "<msgID>_<name>" — digit-prefixed — and both the
// downloader's verify glob and the viewer's prefix scan anchor on that digit
// prefix; the old-layout media scan regex excludes it by the same property.
func WriteDialogMarker(root string, d store.Dialog) error {
	dir := filepath.Join(root, mediaDirName, strconv.FormatInt(d.DialogID, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	// scrub newlines from the values so each key stays exactly one line:
	// titles are user-controlled text and an embedded '\n' would otherwise
	// forge extra lines (usernames cannot carry one, but the defense is free)
	scrub := strings.NewReplacer("\n", " ", "\r", " ")
	var b strings.Builder
	fmt.Fprintf(&b, "dialog_id: %d\n", d.DialogID)
	if d.Title != "" {
		fmt.Fprintf(&b, "title: %s\n", scrub.Replace(d.Title))
	}
	if d.Username != "" {
		fmt.Fprintf(&b, "username: %s\n", scrub.Replace(d.Username))
	}
	if err := os.WriteFile(filepath.Join(dir, markerName), []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Join(dir, markerName), err)
	}
	return nil
}

// writeDialogMarker is the best-effort post-upsert step the import paths
// share: the row is read back first so UpsertDialog's non-clobber merge — not
// this run's possibly-empty fields — decides what the marker says, and a
// failure only warns on stderr because no archive operation may fail over it.
func (a *Archive) writeDialogMarker(dialogID int64) {
	d, ok, err := a.store.GetDialog(dialogID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[archive] dialog marker: %v\n", err)
		return
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "[archive] dialog marker: dialog %d missing right after upsert\n", dialogID)
		return
	}
	if err := WriteDialogMarker(a.cfg.Dir, d); err != nil {
		fmt.Fprintf(os.Stderr, "[archive] dialog marker: %v\n", err)
	}
}

// DownloadResult summarizes a download run.
type DownloadResult struct {
	Done   int
	Failed int
	Passes int
}

// Download downloads a single dialog's pending media smallest-first, in
// batches, verifying each file and retrying up to MaxAttempts across passes.
// Which dialog to serve is resolved via resolveDialogID. It returns when
// nothing is pending, or ctx is canceled, or tdl fails while making no progress.
func (a *Archive) Download(ctx context.Context) (DownloadResult, error) {
	if err := a.runner.Check(ctx); err != nil {
		return DownloadResult{}, err
	}

	dialogID, err := a.resolveDialogID()
	if err != nil {
		return DownloadResult{}, err
	}
	a.dialogID = dialogID

	var res DownloadResult
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		pending, err := a.store.ListPending(dialogID)
		if err != nil {
			return res, err
		}
		if len(pending) == 0 {
			break
		}

		res.Passes++
		fmt.Printf("[archive] pass %d: %d pending (smallest first)\n", res.Passes, len(pending))

		doneThisPass, lastBatchErr, err := a.runBatches(ctx, dialogID, chunk(pending, a.cfg.BatchSize))
		if err != nil {
			return res, err
		}
		res.Done += doneThisPass

		// termination guard: if a pass made zero progress purely because tdl
		// failed, stop instead of looping forever on an infra problem.
		if doneThisPass == 0 && lastBatchErr != nil {
			return res, fmt.Errorf("tdl failed and no files were downloaded this pass (%w); fix the cause and rerun", lastBatchErr)
		}
	}

	counts, err := a.store.CountsAll()
	if err == nil {
		res.Failed = counts[store.StatusFailed]
	}
	return res, nil
}

// TaskRef names one download task to retry, in the "<dialog_id>/<msg_id>"
// form the download --retry flag takes.
type TaskRef struct {
	DialogID int64
	MsgID    int
}

// RetryTasks re-downloads the named tasks regardless of their prior status:
// each is flipped back to pending with its attempts zeroed, then downloaded in
// a single pass of the ordinary batch machinery, grouped by dialog. tdl's
// --skip-same keeps re-verifying an already-intact file cheap. Unlike Download
// there is no outer retry loop: a retry is one pass, and what still fails is
// reported as usual.
func (a *Archive) RetryTasks(ctx context.Context, refs []TaskRef) (DownloadResult, error) {
	var res DownloadResult
	if len(refs) == 0 {
		return res, nil
	}
	if err := a.runner.Check(ctx); err != nil {
		return res, err
	}

	// group by dialog, keeping first-seen order so the run follows the input
	byDialog := map[int64][]int{}
	var order []int64
	for _, ref := range refs {
		if _, seen := byDialog[ref.DialogID]; !seen {
			order = append(order, ref.DialogID)
		}
		byDialog[ref.DialogID] = append(byDialog[ref.DialogID], ref.MsgID)
	}

	// validate every ref before touching any state: a typo in the last entry
	// must not leave the earlier ones half-reset
	for _, dialogID := range order {
		for _, msgID := range byDialog[dialogID] {
			if _, ok, err := a.store.Attempts(dialogID, msgID); err != nil {
				return res, err
			} else if !ok {
				return res, fmt.Errorf("no task %d/%d in state; check `tgxiv status` for the dialog and message ids", dialogID, msgID)
			}
		}
	}

	for _, dialogID := range order {
		msgIDs := byDialog[dialogID]

		for _, msgID := range msgIDs {
			if _, err := a.store.ResetTask(dialogID, msgID); err != nil {
				return res, err
			}
		}

		// re-read exactly the retried records: ListPending sweeps the whole
		// dialog, so filter it down to our ids. A reset task that does not
		// come back pending means state disagrees with itself — surface it
		// instead of silently skipping the retry.
		pending, err := a.store.ListPending(dialogID)
		if err != nil {
			return res, err
		}
		want := msgIDSet(msgIDs)
		var records []store.Record
		for _, r := range pending {
			if want[r.MsgID] {
				records = append(records, r)
				delete(want, r.MsgID)
			}
		}
		for msgID := range want {
			return res, fmt.Errorf("task %d/%d did not return to pending after reset; state is inconsistent", dialogID, msgID)
		}

		res.Passes++
		done, lastBatchErr, err := a.runBatches(ctx, dialogID, chunk(records, a.cfg.BatchSize))
		if err != nil {
			return res, err
		}
		res.Done += done
		// same guard as a Download pass: a no-progress infra failure must not
		// read as "retry done, all good"
		if done == 0 && lastBatchErr != nil {
			return res, fmt.Errorf("tdl failed and no files were downloaded (%w); fix the cause and rerun", lastBatchErr)
		}
	}

	// Failed counts only the retried tasks, intersected per dialog with the
	// failed list: unrelated failures of other runs must not pollute the
	// retry report.
	for _, dialogID := range order {
		failed, err := a.store.ListFailed(dialogID)
		if err != nil {
			return res, err
		}
		want := msgIDSet(byDialog[dialogID])
		for _, r := range failed {
			if want[r.MsgID] {
				res.Failed++
			}
		}
	}
	return res, nil
}

// runBatches drives each batch through tdl (runBatch) and settles the task
// status of every record in it: a size-verified file is MarkDone'd, a missing
// or wrong-size file burns an attempt, an idle-killed batch is charged, and an
// infra failure leaves the task pending for a later pass. It returns how many
// records verified done and the last batch error (nil when every batch ran
// clean). Download's multi-pass loop and RetryTasks' single pass share it, so
// the settling rules exist exactly once.
func (a *Archive) runBatches(ctx context.Context, dialogID int64, batches [][]store.Record) (done int, lastBatchErr error, err error) {
	mediaDir := a.cfg.dialogMediaDir(dialogID)
	for _, batch := range batches {
		if err := ctx.Err(); err != nil {
			return done, lastBatchErr, err
		}

		cmdErr := a.runBatch(ctx, dialogID, batch)
		if cmdErr != nil && errors.Is(cmdErr, context.Canceled) {
			return done, lastBatchErr, cmdErr
		}
		if cmdErr != nil {
			lastBatchErr = cmdErr
			note := "will not count as a file attempt"
			if errors.Is(cmdErr, ErrIdleTimeout) {
				note = "counts as a file attempt"
			}
			fmt.Printf("[archive] tdl batch error (%s): %v\n", note, cmdErr)
		}

		for _, r := range batch {
			vr, err := verify(mediaDir, r.MsgID, r.Size)
			if err != nil {
				return done, lastBatchErr, err
			}
			switch {
			case vr.matched:
				if err := a.store.MarkDone(dialogID, r.MsgID, vr.actualSize, a.mediaStorePath(dialogID, vr.path)); err != nil {
					return done, lastBatchErr, err
				}
				done++
			case cmdErr != nil && errors.Is(cmdErr, ErrIdleTimeout):
				// the stall burned the batch's wall clock; charge it so a
				// permanently stuck item fails instead of stalling reruns
				if err := a.store.MarkAttempt(dialogID, r.MsgID, vr.actualSize, "tdl idle timeout", a.cfg.MaxAttempts); err != nil {
					return done, lastBatchErr, err
				}
			case cmdErr != nil:
				// infra failure: leave pending, do not burn an attempt
			default:
				// tdl succeeded but the file is missing or wrong size
				msg := fmt.Sprintf("expected %d bytes, got %d (%s)", r.Size, vr.actualSize, describeMiss(vr))
				if err := a.store.MarkAttempt(dialogID, r.MsgID, vr.actualSize, msg, a.cfg.MaxAttempts); err != nil {
					return done, lastBatchErr, err
				}
			}
		}
	}
	return done, lastBatchErr, nil
}

// mediaStorePath converts an on-disk media file path into the root-relative
// slash form stored in the DB ("media/<dialogID>/<name>"), so reports and the
// viewer stay independent of where the archive root lives. A path that cannot
// be related back to the root keeps the dialog-folder form of its basename,
// which is still unique within the dialog's media dir.
func (a *Archive) mediaStorePath(dialogID int64, path string) string {
	if rel, err := filepath.Rel(a.cfg.Dir, path); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(filepath.Join(mediaDirName, strconv.FormatInt(dialogID, 10), filepath.Base(path)))
}

// runBatch writes the batch JSON and invokes tdl dl over it, both scoped to
// the dialog: batch.json lives in export/<dialogID>/ and files land in
// media/<dialogID>/ named per dlTemplate.
func (a *Archive) runBatch(ctx context.Context, dialogID int64, batch []store.Record) error {
	exportDir := a.cfg.dialogExportDir(dialogID)
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", exportDir, err)
	}
	batchFile := filepath.Join(exportDir, "batch.json")
	if err := writeBatch(batchFile, dialogID, batch); err != nil {
		return err
	}
	defer func() { _ = os.Remove(batchFile) }()

	mediaDir := a.cfg.dialogMediaDir(dialogID)
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", mediaDir, err)
	}

	opts := tdlx.DownloadOptions{
		BatchFile: batchFile,
		Dir:       mediaDir,
		Threads:   a.cfg.Threads,
		Limit:     a.cfg.Limit,
		// batches are written smallest-first; keep-order makes tdl honor that
		// instead of re-sorting by message id, giving strict size ordering.
		KeepOrder: true,
		// "<msgID>_<name>": the dialog's folder carries the dialog id now
		Template: dlTemplate,
	}

	if a.cfg.IdleTimeout <= 0 {
		return a.runner.Download(ctx, opts)
	}

	// Idle watchdog: tdl can hang forever on one media item (0-byte .tmp,
	// idle TCP). Kill the batch once the dialog's media dir stops growing for
	// IdleTimeout, so the batch's finished files still get verified and
	// committed instead of waiting on the stuck one. Watching the dialog's
	// own dir, not the whole media/ tree, keeps another dialog's activity
	// from keeping a stalled batch alive.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var idleErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		lastGrow := time.Now()
		lastBytes, err := dirBytes(mediaDir)
		if err != nil {
			lastBytes = 0
		}
		ticker := time.NewTicker(a.idleSample)
		defer ticker.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-ticker.C:
			}
			// parent cancel is the user stopping the run, never an idle kill
			if ctx.Err() != nil {
				return
			}
			cur, err := dirBytes(mediaDir)
			if err != nil {
				continue // our own I/O hiccup must not kill a live tdl
			}
			if cur > lastBytes {
				lastBytes, lastGrow = cur, time.Now()
				continue
			}
			if time.Since(lastGrow) >= a.cfg.IdleTimeout {
				idleErr = fmt.Errorf("tdl batch idle for %s (no media bytes written): %w", a.cfg.IdleTimeout, ErrIdleTimeout)
				cancel()
				return
			}
		}
	}()

	err := a.runner.Download(wctx, opts)
	cancel() // runner is done: stop the watchdog...
	<-done   // ...and join it before returning, so batches leak no goroutines
	// the watchdog can fire just after tdl finished everything; a successful
	// run wins over the late idle flag
	if err != nil && idleErr != nil {
		return idleErr // replaces the runner's bare context.Canceled
	}
	return err
}

// dirBytes sums the sizes of all regular files under dir, ".tmp" in-progress
// files included: while tdl is writing, the total keeps growing, so a stall is
// detectable without knowing which file tdl is on. A missing dir reads as 0.
func dirBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return total, err
}

// resolveDialogID pins down which dialog of the multi-dialog DB an operation
// addresses: the dialog of the last Import wins when set, and anything else
// defers to resolveChatDialogID.
func (a *Archive) resolveDialogID() (int64, error) {
	if a.dialogID != 0 {
		return a.dialogID, nil
	}
	return a.resolveChatDialogID()
}

// ResolveChatDialog resolves the --chat flag to a dialog id for commands that
// filter by dialog (status, reset-failed). The bool is false when no --chat is
// configured, meaning "operate on every dialog"; a configured --chat that
// matches nothing is an actionable error, because silently reporting on the
// wrong scope is worse than stopping.
func (a *Archive) ResolveChatDialog() (int64, bool, error) {
	if a.cfg.Chat == "" {
		return 0, false, nil
	}
	id, err := a.resolveChatDialogID()
	if err != nil {
		return 0, true, err
	}
	return id, true, nil
}

// resolveChatDialogID is resolveDialogID without the last-operation cache:
// which dialog does --chat address? A numeric --chat addresses a dialog
// directly — and an unknown id is an error, never a silent fall-through to the
// sole dialog, because exporting dialog A against dialog B's watermark would
// skip real messages. A non-numeric --chat matches a dialog's stored username
// case-insensitively (leading '@' optional), which is what makes
// "sync --chat somechannel" incremental on a multi-dialog root. Otherwise an
// archive holding exactly one dialog is unambiguous; anything else needs the
// user to disambiguate, and the error names the known dialogs to point --chat
// at.
func (a *Archive) resolveChatDialogID() (int64, error) {
	dialogs, err := a.store.ListDialogs()
	if err != nil {
		return 0, err
	}
	if a.cfg.Chat != "" {
		if id, err := strconv.ParseInt(a.cfg.Chat, 10, 64); err == nil {
			for _, d := range dialogs {
				if d.DialogID == id {
					return id, nil
				}
			}
			return 0, fmt.Errorf("--chat %d matches no dialog in state; run export first or check the id", id)
		}
		want := strings.TrimPrefix(a.cfg.Chat, "@")
		for _, d := range dialogs {
			if d.Username != "" && strings.EqualFold(d.Username, want) {
				return d.DialogID, nil
			}
		}
		return 0, fmt.Errorf("--chat %s matches no dialog username in state; run export first or use the dialog id", a.cfg.Chat)
	}
	if len(dialogs) == 1 {
		return dialogs[0].DialogID, nil
	}
	if len(dialogs) == 0 {
		return 0, fmt.Errorf("no dialogs in state; run export first")
	}
	ids := make([]string, len(dialogs))
	for i, d := range dialogs {
		ids[i] = strconv.FormatInt(d.DialogID, 10)
	}
	return 0, fmt.Errorf("archive holds %d dialogs; run export first, or point --chat at one of: %s",
		len(dialogs), strings.Join(ids, ", "))
}

func describeMiss(vr verifyResult) string {
	if vr.path == "" {
		return "no file found"
	}
	return "size mismatch"
}

// batchMessage is one entry of the JSON handed to "tdl dl -f". Only id/type and
// a non-empty file are needed to pass tdl's media filter; tdl re-fetches the
// real media by id.
type batchMessage struct {
	ID   int    `json:"id"`
	Type string `json:"type"`
	File string `json:"file"`
}

type batchFileContent struct {
	ID       int64          `json:"id"`
	Messages []batchMessage `json:"messages"`
}

func writeBatch(path string, channelID int64, batch []store.Record) error {
	content := batchFileContent{ID: channelID, Messages: make([]batchMessage, 0, len(batch))}
	for _, r := range batch {
		file := r.FileName
		if file == "" {
			file = "media"
		}
		content.Messages = append(content.Messages, batchMessage{ID: r.MsgID, Type: "message", File: file})
	}
	b, err := json.Marshal(content)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func chunk(recs []store.Record, size int) [][]store.Record {
	if size <= 0 {
		size = len(recs)
	}
	var out [][]store.Record
	for i := 0; i < len(recs); i += size {
		end := i + size
		if end > len(recs) {
			end = len(recs)
		}
		out = append(out, recs[i:end])
	}
	return out
}

// msgIDSet is the membership helper the retry flow uses to intersect refs with
// store listings.
func msgIDSet(msgIDs []int) map[int]bool {
	set := make(map[int]bool, len(msgIDs))
	for _, id := range msgIDs {
		set[id] = true
	}
	return set
}
