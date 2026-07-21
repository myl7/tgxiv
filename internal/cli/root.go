// Package cli wires the archive commands together.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/myl7/tg-channel-archive/internal/archive"
)

// shared flags, resolved in PersistentPreRun with env fallbacks
var (
	flagDir       string
	flagNamespace string
	flagTdlBin    string
	flagChat      string
)

// envOr returns the flag value if non-empty, else the env var, else def.
func envOr(flagVal, env, def string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

// NewRoot builds the root command tree.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "tgca",
		Short:         "Archive a Telegram channel with tdl: export the manifest, then download media smallest-first with size verification and resume.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVarP(&flagDir, "dir", "d", "", "archive directory (env TGCA_DIR)")
	root.PersistentFlags().StringVarP(&flagNamespace, "ns", "n", "", "tdl session namespace (env TGCA_NS, default \"default\")")
	root.PersistentFlags().StringVar(&flagTdlBin, "tdl", "", "tdl executable (env TGCA_TDL, default \"tdl\")")
	root.PersistentFlags().StringVarP(&flagChat, "chat", "c", "", "channel username, id, or link (env TGCA_CHAT)")

	root.AddCommand(
		newExportCmd(),
		newDownloadCmd(),
		newSyncCmd(),
		newUpdateCmd(),
		newStatusCmd(),
		newImportCmd(),
		newResetFailedCmd(),
	)
	return root
}

// baseConfig assembles the parts of archive.Config that come from shared flags.
func baseConfig() (archive.Config, error) {
	dir := envOr(flagDir, "TGCA_DIR", "")
	if dir == "" {
		return archive.Config{}, fmt.Errorf("archive directory is required (--dir or TGCA_DIR)")
	}
	return archive.Config{
		Dir:       dir,
		Chat:      envOr(flagChat, "TGCA_CHAT", ""),
		Namespace: envOr(flagNamespace, "TGCA_NS", "default"),
		TdlBin:    envOr(flagTdlBin, "TGCA_TDL", "tdl"),
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
