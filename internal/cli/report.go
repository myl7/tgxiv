package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/myl7/tgxiv/internal/archive"
)

// writeFailedReport writes the current list of permanently failed messages of
// the dialog the archive last operated on to a timestamped file under the
// archive's logs dir. It is a no-op when nothing has failed.
func writeFailedReport(a *archive.Archive) error {
	dialogID := a.DialogID()
	if dialogID == 0 {
		return nil // no Import or Download ran; there is no dialog to report on
	}
	failed, err := a.Store().ListFailed(dialogID)
	if err != nil {
		return err
	}
	if len(failed) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %d failed messages\n", len(failed))
	fmt.Fprintf(&b, "# msg_id\tsize\tmedia_type\tfile\n")
	for _, r := range failed {
		fmt.Fprintf(&b, "%d\t%d\t%s\t%s\n", r.MsgID, r.Size, r.MediaType, r.FileName)
	}

	name := filepath.Join(a.LogsDir(), "failed-"+timestamp()+".txt")
	if err := os.WriteFile(name, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("[archive] %d failed messages; report written to %s\n", len(failed), name)
	return nil
}

func timestamp() string { return time.Now().Format("20060102-150405") }
