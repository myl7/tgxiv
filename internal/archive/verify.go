package archive

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// tempExt matches tdl's in-progress suffix (app/dl iter.go).
const tempExt = ".tmp"

// verifyResult reports what the filesystem holds for one message.
type verifyResult struct {
	matched    bool   // a finished file exists at exactly the expected size
	actualSize int64  // size of the located file, 0 if none
	path       string // located file path, empty if none
}

// verify looks for the finished download of message msgID in the dialog's
// media dir. tdl names files "<msgID>_<name>" there (dlTemplate), so the
// prefix locates the file regardless of how the name was sanitized or whether
// the extension was rewritten. In-progress ".tmp" files are ignored. The
// trailing underscore in the prefix prevents msg 1 from matching msg 10.
func verify(dir string, msgID int, expectedSize int64) (verifyResult, error) {
	prefix := formatPrefix(msgID)
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"*"))
	if err != nil {
		return verifyResult{}, err
	}

	var res verifyResult
	for _, m := range matches {
		if strings.HasSuffix(m, tempExt) {
			continue
		}
		info, err := os.Stat(m)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return verifyResult{}, err
		}
		// remember the first finished file for error reporting
		if res.path == "" {
			res.path, res.actualSize = m, info.Size()
		}
		if info.Size() == expectedSize {
			return verifyResult{matched: true, actualSize: info.Size(), path: m}, nil
		}
	}
	return res, nil
}

// formatPrefix builds the literal "<msgID>_" filename prefix.
func formatPrefix(msgID int) string {
	return strconv.Itoa(msgID) + "_"
}
