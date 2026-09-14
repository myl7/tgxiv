package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/myl7/tgxiv/internal/archive"
	"github.com/myl7/tgxiv/internal/store"
)

// download tunables, shared by download and sync
type dlFlags struct {
	batch    int
	attempts int
	threads  int
	limit    int
}

func (f *dlFlags) bind(cmd *cobra.Command) {
	cmd.Flags().IntVar(&f.batch, "batch", 100, "messages per tdl dl invocation (1 = one message per call)")
	cmd.Flags().IntVar(&f.attempts, "attempts", 3, "download attempts per message before giving up")
	cmd.Flags().IntVar(&f.threads, "threads", 0, "tdl --threads (0 = tdl default)")
	cmd.Flags().IntVar(&f.limit, "limit", 0, "tdl --limit concurrent files (0 = tdl default)")
}

func (f *dlFlags) apply(c *archive.Config) {
	c.BatchSize = f.batch
	c.MaxAttempts = f.attempts
	c.Threads = f.threads
	c.Limit = f.limit
}

func newManifestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "manifest",
		Short: "Full tdl export for the channel; import its media manifest into the state DB",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openArchive(nil)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			if a.Chat() == "" {
				return fmt.Errorf("channel is required (--chat or TGXIV_CHAT)")
			}

			ctx, cancel := signalContext()
			defer cancel()

			res, err := a.Export(ctx, false)
			if err != nil {
				return err
			}
			printExportResult(res)
			return nil
		},
	}
}

func newDownloadCmd() *cobra.Command {
	var f dlFlags
	var retries []string
	cmd := &cobra.Command{
		Use:     "download",
		Aliases: []string{"dl"},
		Short:   "Download pending media smallest-first, verifying size and retrying; --retry <dialog_id>/<msg_id> re-runs specific tasks instead",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openArchive(f.apply)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			ctx, cancel := signalContext()
			defer cancel()

			// --retry replaces the normal sweep: it re-runs exactly the named
			// tasks regardless of their prior status
			if len(retries) > 0 {
				refs, dialogIDs, err := parseTaskRefs(retries)
				if err != nil {
					return err
				}
				res, err := a.RetryTasks(ctx, refs)
				printDownloadResult(res)
				if err != nil {
					return err
				}
				return writeFailedReports(a, dialogIDs)
			}

			res, err := a.Download(ctx)
			printDownloadResult(res)
			if err != nil {
				return err
			}
			return writeFailedReports(a, []int64{a.DialogID()})
		},
	}
	f.bind(cmd)
	cmd.Flags().StringSliceVar(&retries, "retry", nil,
		"re-download specific tasks as <dialog_id>/<msg_id> instead of all pending media (repeatable, or comma-separated)")
	return cmd
}

// parseTaskRefs parses --retry entries of the form "<dialog_id>/<msg_id>" and
// returns the refs plus their distinct dialog ids in first-seen order — the
// set a retry's failed-report must cover. A malformed entry is an actionable
// error: a silent skip would look exactly like a successful retry.
func parseTaskRefs(entries []string) ([]archive.TaskRef, []int64, error) {
	var refs []archive.TaskRef
	seen := map[int64]bool{}
	var dialogIDs []int64
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		dialogS, msgS, ok := strings.Cut(entry, "/")
		if !ok {
			return nil, nil, fmt.Errorf("--retry %q must be <dialog_id>/<msg_id>, e.g. 3992083278/2914", entry)
		}
		dialogID, err := strconv.ParseInt(strings.TrimSpace(dialogS), 10, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("--retry %q: the dialog id must be an integer: %w", entry, err)
		}
		msgID, err := strconv.Atoi(strings.TrimSpace(msgS))
		if err != nil {
			return nil, nil, fmt.Errorf("--retry %q: the message id must be an integer: %w", entry, err)
		}
		refs = append(refs, archive.TaskRef{DialogID: dialogID, MsgID: msgID})
		if !seen[dialogID] {
			seen[dialogID] = true
			dialogIDs = append(dialogIDs, dialogID)
		}
	}
	return refs, dialogIDs, nil
}

func newArchiveCmd() *cobra.Command {
	var f dlFlags
	cmd := &cobra.Command{
		Use:   "archive",
		Short: "Full export + download in one step (use for periodic reconciliation)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return exportThenDownload(&f, false)
		},
	}
	f.bind(cmd)
	return cmd
}

func newSyncCmd() *cobra.Command {
	var f dlFlags
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Incremental: fetch and download only messages newer than the last sync",
		RunE: func(cmd *cobra.Command, args []string) error {
			return exportThenDownload(&f, true)
		},
	}
	f.bind(cmd)
	return cmd
}

// exportThenDownload runs an export (full or incremental) followed by a download.
// It is the shared body of the archive and sync commands.
func exportThenDownload(f *dlFlags, incremental bool) error {
	a, err := openArchive(f.apply)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	if a.Chat() == "" {
		return fmt.Errorf("channel is required (--chat or TGXIV_CHAT)")
	}

	ctx, cancel := signalContext()
	defer cancel()

	res, err := a.Export(ctx, incremental)
	if err != nil {
		return err
	}
	printExportResult(res)

	if res.Added == 0 && incremental {
		fmt.Println("[archive] no new media to download")
		return nil
	}

	dres, err := a.Download(ctx)
	printDownloadResult(dres)
	if err != nil {
		return err
	}
	return writeFailedReports(a, []int64{a.DialogID()})
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show manifest and download status (all dialogs, or --chat for one dialog's block)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openArchive(nil)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			// --chat narrows the report to one dialog's block; without it the
			// global rollup plus every dialog's block is printed as before
			dialogID, filtered, err := a.ResolveChatDialog()
			if err != nil {
				return err
			}

			dialogs, err := a.Store().CountsByDialog()
			if err != nil {
				return err
			}

			if filtered {
				dc, err := dialogCounts(a, dialogs, dialogID)
				if err != nil {
					return err
				}
				return printDialogBlock(a, dc)
			}

			// global rollup across every dialog in the archive
			counts, err := a.Store().CountsAll()
			if err != nil {
				return err
			}
			fmt.Printf("total:   %d\n", counts["total"])
			fmt.Printf("done:    %d\n", counts[store.StatusDone])
			fmt.Printf("pending: %d\n", counts[store.StatusPending])
			fmt.Printf("failed:  %d\n", counts[store.StatusFailed])

			for _, dc := range dialogs {
				fmt.Println()
				if err := printDialogBlock(a, dc); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// dialogCounts finds the dialog's task rollup among the per-dialog counts. A
// dialog with no tasks yet has no row there, so fall back to its dialog row
// with zeroed counts — the block still names the dialog instead of vanishing.
func dialogCounts(a *archive.Archive, dialogs []store.DialogCounts, dialogID int64) (store.DialogCounts, error) {
	for _, dc := range dialogs {
		if dc.Dialog.DialogID == dialogID {
			return dc, nil
		}
	}
	d, ok, err := a.Store().GetDialog(dialogID)
	if err != nil {
		return store.DialogCounts{}, err
	}
	if !ok {
		return store.DialogCounts{}, fmt.Errorf("dialog %d not in state; run export first", dialogID)
	}
	return store.DialogCounts{Dialog: d}, nil
}

// printDialogBlock prints one dialog's status block, shared by the filtered
// and unfiltered paths so their output stays identical and grep-friendly.
func printDialogBlock(a *archive.Archive, dc store.DialogCounts) error {
	fmt.Printf("dialog %s\n", dialogLabel(dc.Dialog))
	fmt.Printf("  total:   %d\n", dc.Total)
	fmt.Printf("  done:    %d\n", dc.Done)
	fmt.Printf("  pending: %d\n", dc.Pending)
	fmt.Printf("  failed:  %d\n", dc.Failed)

	failed, err := a.Store().ListFailed(dc.Dialog.DialogID)
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		fmt.Printf("\n  failed messages:\n")
		// ListFailed is already ordered by msg_id
		for _, r := range failed {
			fmt.Printf("  %d  %s  (%d bytes)\n", r.MsgID, r.FileName, r.Size)
		}
	}
	return nil
}

// dialogLabel renders a dialog header: the id, plus title and @username when
// known, so a multi-dialog status stays readable while staying grep-friendly.
func dialogLabel(d store.Dialog) string {
	var parts []string
	if d.Title != "" {
		parts = append(parts, d.Title)
	}
	if d.Username != "" {
		parts = append(parts, "@"+d.Username)
	}
	if len(parts) == 0 {
		return strconv.FormatInt(d.DialogID, 10)
	}
	return fmt.Sprintf("%d (%s)", d.DialogID, strings.Join(parts, " "))
}

func newResetFailedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset-failed",
		Short: "Flip failed messages back to pending so they are retried (all dialogs, or --chat for one)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openArchive(nil)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			dialogID, filtered, err := a.ResolveChatDialog()
			if err != nil {
				return err
			}
			if filtered {
				n, err := a.Store().ResetFailed(dialogID)
				if err != nil {
					return err
				}
				fmt.Printf("reset %d failed messages to pending (dialog %d)\n", n, dialogID)
				return nil
			}
			n, err := a.Store().ResetFailedAll()
			if err != nil {
				return err
			}
			fmt.Printf("reset %d failed messages to pending (all dialogs)\n", n)
			return nil
		},
	}
}

func printDownloadResult(res archive.DownloadResult) {
	fmt.Printf("[archive] done: %d downloaded, %d failed, %d passes\n", res.Done, res.Failed, res.Passes)
}

func printExportResult(res archive.ExportResult) {
	kind := "full export"
	if res.Incremental {
		kind = fmt.Sprintf("incremental export (id >= %d)", res.SinceID)
	}
	fmt.Printf("[archive] %s -> %s; %d new media messages added\n", kind, res.File, res.Added)
}
