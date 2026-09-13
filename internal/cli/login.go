package cli

import (
	"github.com/spf13/cobra"

	"github.com/myl7/tgxiv/internal/tdlx"
)

// newLoginCmd shells out to "tdl login" against the same namespace tgxiv uses
// for export and download. The session lives in ~/.tdl and is shared with any
// tdl binary on the same namespace: "tdl login" and "tgxiv login" are
// interchangeable.
func newLoginCmd() *cobra.Command {
	var code bool

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Login to Telegram and store the session in the tdl namespace (QR code by default)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ns := envOr(flagNamespace, []string{"TGXIV_NS", "TGCA_NS"}, "default")
			r := &tdlx.Runner{
				Bin:       envOr(flagTdl, []string{"TGXIV_TDL"}, "tdl"),
				Namespace: ns,
			}
			return r.Login(cmd.Context(), code)
		},
	}

	cmd.Flags().BoolVar(&code, "code", false, "login with phone number and verification code instead of QR code")
	return cmd
}
