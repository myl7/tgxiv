package archive

import (
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"

	"github.com/myl7/tgxiv/internal/store"
)

// SanitizeFileName makes a media-provided filename safe for the local
// filesystem. tasks.file_name is uploader-controlled text: Telegram's own
// rules do not guarantee OUR filesystem accepts it. Path separators (/ and
// \), control bytes (<0x20), and the Windows-forbidden set (< > : " | ? *)
// become '_'; trailing dots and spaces are trimmed (Windows strips them on
// create, which would desync the predicted name from the name that lands on
// disk). The result may be "" — the name had nothing usable. There is
// deliberately NO length handling: modern Windows commonly has long-path
// support, so a conservative byte budget would silently rename names the
// filesystem would accept, and a root path that IS too deep is the user's
// setup — honestly surfaced as a download failure (see longPathHint) rather
// than preemptively truncated files. Windows reserved device names (CON, PRN,
// ...) need no handling of their own: the pipeline prefixes every download
// with "<msgID>_", and "123_CON" is not a reserved name.
func SanitizeFileName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for i := 0; i < len(name); i++ {
		if badNameByte(name[i]) {
			b.WriteByte('_')
		} else {
			b.WriteByte(name[i])
		}
	}
	return strings.TrimRight(b.String(), ". ")
}

// badNameByte reports whether a byte can never appear in a portable filename
// component: the two separators, C0 control bytes, and the characters Windows
// forbids outright. Every one of them is single-byte ASCII, so byte-wise
// replacement never splits a UTF-8 rune.
func badNameByte(c byte) bool {
	return c == '/' || c == '\\' || c < 0x20 ||
		c == '<' || c == '>' || c == ':' || c == '"' || c == '|' || c == '?' || c == '*'
}

// diskNameFor decides the base name (WITHOUT the "<msgID>_" prefix) a media
// record must use on disk: "" — "identical to file_name" — whenever the
// media-provided name needs no change (the default for every sane name, which
// also keeps long-standing on-disk files matching, so tdl's --skip-same keeps
// re-verifying them cheaply); the sanitized form when the name needs any
// change; a UUID fallback when nothing usable survives sanitization.
func diskNameFor(name string) string {
	switch s := SanitizeFileName(name); {
	case s == "":
		// nameless or garbage-only: a UUID name, keeping any extension
		// salvageable from the original
		return UUIDName(salvageExt(name))
	case s != name:
		return s
	}
	return ""
}

// salvageExt pulls a short extension off the ORIGINAL media-provided name for
// the UUID fallback: a salvaged ".jpg" keeps the file's kind recognizable to
// the viewer without the rest of the unusable name. The extension is uploader
// text like everything else, so it goes through the same character rules.
func salvageExt(name string) string {
	return SanitizeFileName(strings.Trim(filepath.Ext(name), "."))
}

// UUIDName is the fallback disk name for a media record whose provided name
// has nothing usable: a UUID v4 as 32 lowercase hex chars — 16 crypto/rand
// bytes with the RFC 4122 version and variant bits set by hand, because a
// dependency for three lines of bit-twiddling is not worth carrying — plus
// ".<ext>" when an extension was given. Uniqueness on disk does not ride on
// the UUID alone: the "<msgID>_" prefix the pipeline adds already guarantees
// one name per message.
func UUIDName(ext string) string {
	var b [16]byte
	// crypto/rand's Read never fails on the Go versions tgxiv builds for (a
	// broken entropy source panics in the runtime instead), so the error is
	// deliberately dropped.
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // variant 10x1
	name := hex.EncodeToString(b[:])
	if ext != "" {
		return name + "." + ext
	}
	return name
}

// maxLegacyPathUnits is the classic Windows MAX_PATH: 260 UTF-16 code units
// (drive + colon + slash + 256 name units + NUL). It remains the EFFECTIVE
// limit for a Go binary like tgxiv driving tdl even on modern Windows: the
// registry LongPathsEnabled switch defaults to 0, and Go toolchain binaries
// embed no longPathAware manifest (golang/go#17835, golang/go#78601), so a
// path past it fails regardless of machine configuration — the one naming
// failure character filtering cannot prevent, detected only to explain it
// (longPathHint).
const maxLegacyPathUnits = 260

// longPathHint explains a missing download whose WOULD-BE on-disk path
// overflows Windows' effective limit: the archive root sits too deep, and no
// name tgxiv could have picked would have fixed that. Windows-only by GOOS:
// macOS and Linux have no whole-path limit anywhere near this scale
// (255-byte components, 1024-byte PATH_MAX), so a >260-unit path there is
// usually perfectly legal and the hint would mislead. It is a diagnostic
// only, appended to the recorded error text of an ordinary failed attempt —
// nothing is renamed or truncated preemptively (see SanitizeFileName for
// why) and the control flow is untouched.
func longPathHint(mediaDir string, r store.Record) string {
	if runtime.GOOS != "windows" {
		return ""
	}
	return longPathSuffix(filepath.Join(mediaDir, formatPrefix(r.MsgID)+batchFileName(r)))
}

// longPathSuffix is longPathHint's threshold test over an already-built
// path: MAX_PATH counts UTF-16 code units, not bytes, so a CJK-heavy path of
// well over 260 bytes can still be legal and must not fire the hint. Split
// out so the unit-counting rule itself stays testable on every platform.
func longPathSuffix(path string) string {
	if len(utf16.Encode([]rune(path))) <= maxLegacyPathUnits {
		return ""
	}
	return "; the archive root path may be too long for this filesystem"
}
