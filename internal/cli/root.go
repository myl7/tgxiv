// Package cli wires the archive commands together.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/myl7/tgxiv/internal/archive"
	"github.com/myl7/tgxiv/internal/migrate"
)

// shared flags, resolved in PersistentPreRun with env fallbacks
var (
	flagDir       string
	flagNamespace string
	flagChat      string
	flagTdl       string
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
	root.PersistentFlags().StringVar(&flagTdl, "tdl", "", "tdl executable (env TGXIV_TDL, default \"tdl\")")

	root.AddCommand(
		newLoginCmd(),
		newArchiveCmd(),
		newSyncCmd(),
		newManifestCmd(),
		newDownloadCmd(),
		newStatusCmd(),
		newResetFailedCmd(),
		newServeCmd(),
		migrate.New(baseConfig),
	)
	return root
}

// baseConfig assembles the parts of archive.Config that come from shared flags.
func baseConfig() (archive.Config, error) {
	dir := envOr(flagDir, []string{"TGXIV_DIR", "TGCA_DIR"}, "")
	if dir == "" {
		return archive.Config{}, fmt.Errorf("archive directory is required (--dir or TGXIV_DIR)")
	}
	return archive.Config{
		Dir:       dir,
		Chat:      envOr(flagChat, []string{"TGXIV_CHAT", "TGCA_CHAT"}, ""),
		Namespace: envOr(flagNamespace, []string{"TGXIV_NS", "TGCA_NS"}, "default"),
		TdlBin:    envOr(flagTdl, []string{"TGXIV_TDL"}, "tdl"),
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
