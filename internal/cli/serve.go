package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/myl7/tgxiv/internal/webui"
)

// newServeCmd serves the bundled web viewer. It takes the archive root —
// the dir holding tgxiv.sqlite and media/ — via the shared --dir flag,
// since the single-db model gives the viewer and the archiver the same
// view of one dir.
func newServeCmd() *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the bundled web viewer for an archive directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := envOr(flagDir, []string{"TGXIV_DIR"}, "channels")
			listen := envOr(addr, []string{"TGXIV_ADDR"}, "127.0.0.1:8080")

			n, err := webui.CountDialogs(dir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "tgxiv: listing dialogs: %v\n", err)
			}
			fmt.Printf("tgxiv: serving %d dialog(s) from %s at http://%s\n", n, dir, listen)

			srv := &http.Server{
				Addr:    listen,
				Handler: webui.New(dir),
				// no WriteTimeout: a slow client pulling a large media file
				// must not be cut off mid-stream
				ReadHeaderTimeout: 10 * time.Second,
				IdleTimeout:       2 * time.Minute,
			}

			ctx, cancel := signalContext()
			defer cancel()

			errCh := make(chan error, 1)
			go func() { errCh <- srv.ListenAndServe() }()
			select {
			case err := <-errCh:
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				return err
			case <-ctx.Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				return srv.Shutdown(shutdownCtx)
			}
		},
	}

	cmd.Flags().StringVar(&addr, "addr", "", "listen address (env TGXIV_ADDR, default \"127.0.0.1:8080\")")
	return cmd
}
