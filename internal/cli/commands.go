package cli

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/myl7/tg-channel-archive/internal/archive"
	"github.com/myl7/tg-channel-archive/internal/store"
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

func newExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export",
		Short: "Full tdl export for the channel; import its media manifest into the state DB",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openArchive(nil)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			if a.Chat() == "" {
				return fmt.Errorf("channel is required (--chat or TGCA_CHAT)")
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

func newImportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import <export.json>",
		Short: "Import an existing tdl export JSON into the state DB (no tdl call)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openArchive(nil)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			added, err := a.Import(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("imported %s; %d new media messages added to manifest\n", args[0], added)
			return nil
		},
	}
}

func newDownloadCmd() *cobra.Command {
	var f dlFlags
	cmd := &cobra.Command{
		Use:     "download",
		Aliases: []string{"dl"},
		Short:   "Download pending media smallest-first, verifying size and retrying",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openArchive(f.apply)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			ctx, cancel := signalContext()
			defer cancel()

			res, err := a.Download(ctx)
			printDownloadResult(res)
			if err != nil {
				return err
			}
			return writeFailedReport(a)
		},
	}
	f.bind(cmd)
	return cmd
}

func newSyncCmd() *cobra.Command {
	var f dlFlags
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Full export + download in one step (use for periodic reconciliation)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return exportThenDownload(&f, false)
		},
	}
	f.bind(cmd)
	return cmd
}

func newUpdateCmd() *cobra.Command {
	var f dlFlags
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Incremental: fetch and download only messages newer than the last sync",
		RunE: func(cmd *cobra.Command, args []string) error {
			return exportThenDownload(&f, true)
		},
	}
	f.bind(cmd)
	return cmd
}

// exportThenDownload runs an export (full or incremental) followed by a download.
// It is the shared body of the sync and update commands.
func exportThenDownload(f *dlFlags, incremental bool) error {
	a, err := openArchive(f.apply)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	if a.Chat() == "" {
		return fmt.Errorf("channel is required (--chat or TGCA_CHAT)")
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
	return writeFailedReport(a)
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show manifest and download status",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openArchive(nil)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			counts, err := a.Store().Counts()
			if err != nil {
				return err
			}
			fmt.Printf("total:   %d\n", counts["total"])
			fmt.Printf("done:    %d\n", counts[store.StatusDone])
			fmt.Printf("pending: %d\n", counts[store.StatusPending])
			fmt.Printf("failed:  %d\n", counts[store.StatusFailed])

			failed, err := a.Store().ListFailed()
			if err != nil {
				return err
			}
			if len(failed) > 0 {
				fmt.Printf("\nfailed messages:\n")
				sort.Slice(failed, func(i, j int) bool { return failed[i].MsgID < failed[j].MsgID })
				for _, r := range failed {
					fmt.Printf("  %d  %s  (%d bytes)\n", r.MsgID, r.FileName, r.Size)
				}
			}
			return nil
		},
	}
}

func newResetFailedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset-failed",
		Short: "Flip all failed messages back to pending so they are retried",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openArchive(nil)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			n, err := a.Store().ResetFailed()
			if err != nil {
				return err
			}
			fmt.Printf("reset %d failed messages to pending\n", n)
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
