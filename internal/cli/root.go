// Package cli wires the archive commands together.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/myl7/tgxiv/internal/archive"
)

// shared flags, resolved in PersistentPreRun with env fallbacks
var (
	flagDir       string
	flagNamespace string
	flagChat      string
	flagTdl       string
	flagIdle      string
)

// envOr returns the flag value if non-empty, else the first non-empty env var
// in envs (TGXIV_* before legacy TGCA_*), else def.
func envOr(flagVal string, envs []string, def string) string {
	if flagVal != "" {
		return flagVal
	}
	for _, env := range envs {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return def
}

// NewRoot builds the root command tree.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "tgxiv",
		Short:         "Archive a Telegram channel with tdl: export the manifest, then download media smallest-first with size verification and resume.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVarP(&flagDir, "dir", "d", "", "archive directory (env TGXIV_DIR)")
	root.PersistentFlags().StringVarP(&flagNamespace, "ns", "n", "", "tdl session namespace (env TGXIV_NS, default \"default\")")
	root.PersistentFlags().StringVarP(&flagChat, "chat", "c", "", "channel username, id, or link (env TGXIV_CHAT)")
	root.PersistentFlags().StringVar(&flagTdl, "tdl", "", "tdl executable (env TGXIV_TDL; default: PATH, then tdl/tdl.exe in the working directory)")
	root.PersistentFlags().StringVar(&flagIdle, "idle-timeout", "", "kill a stalled tdl dl batch after this long with no bytes written (default 5m; env TGXIV_IDLE_TIMEOUT; 0s disables)")

	root.AddCommand(
		newLoginCmd(),
		newArchiveCmd(),
		newSyncCmd(),
		newManifestCmd(),
		newDownloadCmd(),
		newStatusCmd(),
		newResetFailedCmd(),
		newSplitCmd(),
		newServeCmd(),
	)
	return root
}

// baseConfig assembles the parts of archive.Config that come from shared flags.
func baseConfig() (archive.Config, error) {
	dir := envOr(flagDir, []string{"TGXIV_DIR", "TGCA_DIR"}, "")
	if dir == "" {
		return archive.Config{}, fmt.Errorf("archive directory is required (--dir or TGXIV_DIR)")
	}
	idleRaw := envOr(flagIdle, []string{"TGXIV_IDLE_TIMEOUT"}, "5m")
	idle, err := time.ParseDuration(idleRaw)
	if err != nil {
		return archive.Config{}, fmt.Errorf("invalid idle timeout %q (--idle-timeout or TGXIV_IDLE_TIMEOUT must be a duration like 5m or 0s): %w", idleRaw, err)
	}
	return archive.Config{
		Dir:         dir,
		Chat:        envOr(flagChat, []string{"TGXIV_CHAT", "TGCA_CHAT"}, ""),
		Namespace:   envOr(flagNamespace, []string{"TGXIV_NS", "TGCA_NS"}, "default"),
		TdlBin:      envOr(flagTdl, []string{"TGXIV_TDL"}, "tdl"),
		IdleTimeout: idle,
	}, nil
}

// openArchive builds and opens an Archive from the shared flags plus extras.
func openArchive(extra func(*archive.Config)) (*archive.Archive, error) {
	cfg, err := baseConfig()
	if err != nil {
		return nil, err
	}
	if extra != nil {
		extra(&cfg)
	}
	return archive.Open(cfg)
}

// signalContext returns a context canceled on SIGINT/SIGTERM so a Ctrl-C is
// forwarded to tdl for a clean, resumable stop.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
