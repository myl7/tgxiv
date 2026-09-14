package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/myl7/tgxiv/internal/archive"
)

// writeFailedReports writes one timestamped failed-list file per dialog to
// logs/failed-<dialog_id>-<ts>.txt. dialogIDs is the set of dialogs a run
// actually touched: the single dialog of a normal download, or every dialog a
// --retry run's refs named. A dialog with nothing failed gets no file.
func writeFailedReports(a *archive.Archive, dialogIDs []int64) error {
	for _, dialogID := range dialogIDs {
		if dialogID == 0 {
			continue // no Import or Download ran; there is no dialog to report on
		}
		failed, err := a.Store().ListFailed(dialogID)
		if err != nil {
			return err
		}
		if len(failed) == 0 {
			continue
		}

		var b strings.Builder
		fmt.Fprintf(&b, "# dialog %d: %d failed messages\n", dialogID, len(failed))
		fmt.Fprintf(&b, "# msg_id\tsize\tmedia_type\tfile\n")
		for _, r := range failed {
			fmt.Fprintf(&b, "%d\t%d\t%s\t%s\n", r.MsgID, r.Size, r.MediaType, r.FileName)
		}

		name := filepath.Join(a.LogsDir(), fmt.Sprintf("failed-%d-%s.txt", dialogID, timestamp()))
		if err := os.WriteFile(name, []byte(b.String()), 0o644); err != nil {
			return err
		}
		fmt.Printf("[archive] dialog %d: %d failed messages; report written to %s\n", dialogID, len(failed), name)
	}
	return nil
}

func timestamp() string { return time.Now().Format("20060102-150405") }
