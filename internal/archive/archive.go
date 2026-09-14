// Package archive orchestrates a channel archive: export the manifest with tdl,
// record it in a state DB, then download media smallest-first in batches,
// verifying each file by size and retrying a bounded number of times.
package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/myl7/tgxiv/internal/exportjson"
	"github.com/myl7/tgxiv/internal/media"
	"github.com/myl7/tgxiv/internal/store"
	"github.com/myl7/tgxiv/internal/tdlx"
)

// Config is the archive's static configuration.
type Config struct {
	Dir         string // archive root directory
	Chat        string // channel username, id, or link (for export)
	Namespace   string // tdl session namespace
	TdlBin      string // tdl executable
	BatchSize   int    // messages per tdl dl invocation
	MaxAttempts int    // per-message download attempts before giving up
	Threads     int    // tdl --threads (0 = tdl default)
	Limit       int    // tdl --limit concurrent files (0 = tdl default)
}

// tdlRunner is the slice of tdlx.Runner the archive depends on. It is an
// interface so tests can drive the orchestration without a real tdl binary.
type tdlRunner interface {
	Check(context.Context) error
	Export(context.Context, tdlx.ExportOptions) error
	Download(context.Context, tdlx.DownloadOptions) error
}

// Archive binds a Config to its open state DB and tdl runner.
type Archive struct {
	cfg    Config
	store  *store.Store
	runner tdlRunner
}

// Layout returns the standard sub-paths under the archive dir.
func (c Config) dbPath() string    { return filepath.Join(c.Dir, "archive.db") }
func (c Config) mediaDir() string  { return filepath.Join(c.Dir, "media") }
func (c Config) exportDir() string { return filepath.Join(c.Dir, "export") }
func (c Config) logsDir() string   { return filepath.Join(c.Dir, "logs") }

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

	for _, d := range []string{cfg.Dir, cfg.mediaDir(), cfg.exportDir(), cfg.logsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}

	st, err := store.Open(cfg.dbPath())
	if err != nil {
		return nil, err
	}

	return &Archive{
		cfg:   cfg,
		store: st,
		runner: &tdlx.Runner{
			Bin:       cfg.TdlBin,
			Namespace: cfg.Namespace,
		},
	}, nil
}

// Close releases the state DB.
func (a *Archive) Close() error { return a.store.Close() }

// Store exposes the underlying store for read-only reporting (status command).
func (a *Archive) Store() *store.Store { return a.store }

// Chat returns the configured channel identifier.
func (a *Archive) Chat() string { return a.cfg.Chat }

// LogsDir returns the archive's logs directory.
func (a *Archive) LogsDir() string { return a.cfg.logsDir() }

// ExportResult summarizes an export+import.
type ExportResult struct {
	File        string // export JSON path; transient, removed once imported
	Added       int    // new media messages added to the manifest
	SinceID     int    // id floor passed to tdl; 0 means a full export
	Incremental bool   // an incremental delta was actually applied
}

// Export runs a tdl export into the export dir and imports it into the DB.
// The export file itself is transient: it is deleted once imported. When
// incremental is true it fetches only messages at or above the stored
// watermark; with no watermark yet it falls back to a full export.
func (a *Archive) Export(ctx context.Context, incremental bool) (ExportResult, error) {
	if err := a.runner.Check(ctx); err != nil {
		return ExportResult{}, err
	}

	sinceID := 0
	if incremental {
		wm, err := a.store.LastMsgID()
		if err != nil {
			return ExportResult{}, err
		}
		if wm > 0 {
			sinceID = wm + 1 // strictly newer than what we have
		}
	}

	stamp := time.Now().Format("20060102-150405")
	out := filepath.Join(a.cfg.exportDir(), stamp+".json")

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
	return ExportResult{
		File:        out,
		Added:       added,
		SinceID:     sinceID,
		Incremental: incremental && sinceID > 0,
	}, nil
}

// Import parses a tdl export JSON and records it in the DB: every message gets
// a content row (text-only and service messages included; the DB is the text
// archive), and the media subset is additionally upserted into the download
// manifest.
func (a *Archive) Import(path string) (added int, err error) {
	var contentRecs []store.ContentRecord
	var recs []store.Record
	maxSeen := 0 // highest id of ANY message, for the incremental watermark
	channelID, err := exportjson.ParseFile(path, func(m exportjson.Message) error {
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
			DialogID:  channelIDPlaceholder, // set below once known
			FileName:  name,
			Size:      info.Size,
			MediaType: string(info.Type),
			Date:      m.Date,
		})
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("parse export: %w", err)
	}

	// stamp the resolved channel id onto every record and persist it
	for i := range recs {
		recs[i].DialogID = channelID
	}
	if err := a.store.SetMeta("channel_id", fmt.Sprintf("%d", channelID)); err != nil {
		return 0, err
	}
	if err := a.store.SetMeta("namespace", a.cfg.Namespace); err != nil {
		return 0, err
	}
	if err := a.store.UpsertContent(contentRecs); err != nil {
		return 0, err
	}
	added, err = a.store.UpsertManifest(recs)
	if err != nil {
		return 0, err
	}
	// advance the watermark last: only after content and manifest are committed
	if err := a.store.AdvanceLastMsgID(maxSeen); err != nil {
		return added, err
	}
	return added, nil
}

// channelIDPlaceholder is overwritten before insert; kept explicit for clarity.
const channelIDPlaceholder int64 = 0

// DownloadResult summarizes a download run.
type DownloadResult struct {
	Done   int
	Failed int
	Passes int
}

// Download downloads all pending media smallest-first, in batches, verifying
// each file and retrying up to MaxAttempts across passes. It returns when
// nothing is pending, or ctx is canceled, or tdl fails while making no progress.
func (a *Archive) Download(ctx context.Context) (DownloadResult, error) {
	if err := a.runner.Check(ctx); err != nil {
		return DownloadResult{}, err
	}

	channelID, err := a.channelID()
	if err != nil {
		return DownloadResult{}, err
	}

	var res DownloadResult
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		pending, err := a.store.ListPending()
		if err != nil {
			return res, err
		}
		if len(pending) == 0 {
			break
		}

		res.Passes++
		fmt.Printf("[archive] pass %d: %d pending (smallest first)\n", res.Passes, len(pending))

		doneThisPass := 0
		infraError := false

		for _, batch := range chunk(pending, a.cfg.BatchSize) {
			if err := ctx.Err(); err != nil {
				return res, err
			}

			cmdErr := a.runBatch(ctx, channelID, batch)
			if cmdErr != nil && errors.Is(cmdErr, context.Canceled) {
				return res, cmdErr
			}
			if cmdErr != nil {
				infraError = true
				fmt.Printf("[archive] tdl batch error (will not count as a file attempt): %v\n", cmdErr)
			}

			for _, r := range batch {
				vr, err := verify(a.cfg.mediaDir(), r.DialogID, r.MsgID, r.Size)
				if err != nil {
					return res, err
				}
				switch {
				case vr.matched:
					if err := a.store.MarkDone(r.MsgID, vr.actualSize, vr.path); err != nil {
						return res, err
					}
					res.Done++
					doneThisPass++
				case cmdErr != nil:
					// infra failure: leave pending, do not burn an attempt
				default:
					// tdl succeeded but the file is missing or wrong size
					msg := fmt.Sprintf("expected %d bytes, got %d (%s)", r.Size, vr.actualSize, describeMiss(vr))
					if err := a.store.MarkAttempt(r.MsgID, vr.actualSize, msg, a.cfg.MaxAttempts); err != nil {
						return res, err
					}
				}
			}
		}

		// termination guard: if a pass made zero progress purely because tdl
		// failed, stop instead of looping forever on an infra problem.
		if doneThisPass == 0 && infraError {
			return res, fmt.Errorf("tdl failed and no files were downloaded this pass; fix the cause and rerun")
		}
	}

	counts, err := a.store.Counts()
	if err == nil {
		res.Failed = counts[store.StatusFailed]
	}
	return res, nil
}

// runBatch writes the batch JSON and invokes tdl dl over it.
func (a *Archive) runBatch(ctx context.Context, channelID int64, batch []store.Record) error {
	batchFile := filepath.Join(a.cfg.exportDir(), "batch.json")
	if err := writeBatch(batchFile, channelID, batch); err != nil {
		return err
	}
	defer func() { _ = os.Remove(batchFile) }()

	return a.runner.Download(ctx, tdlx.DownloadOptions{
		BatchFile: batchFile,
		Dir:       a.cfg.mediaDir(),
		Threads:   a.cfg.Threads,
		Limit:     a.cfg.Limit,
		// batches are written smallest-first; keep-order makes tdl honor that
		// instead of re-sorting by message id, giving strict size ordering.
		KeepOrder: true,
	})
}

func (a *Archive) channelID() (int64, error) {
	v, ok, err := a.store.GetMeta("channel_id")
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("no channel id in state; run export first")
	}
	var id int64
	if _, err := fmt.Sscan(v, &id); err != nil {
		return 0, fmt.Errorf("bad channel id %q: %w", v, err)
	}
	return id, nil
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
