// Command tgxiv archives a Telegram channel using tdl as the download engine.
package main

import (
	"fmt"
	"os"

	"github.com/myl7/tgxiv/internal/cli"
)

func main() {
	if err := cli.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
