package archive

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestSanitizeFileName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// the compatibility case: every sane name passes untouched, so
		// long-standing on-disk files keep matching their media names
		{"identity", "clip.mp4", "clip.mp4"},
		{"identity with spaces and parens", "my clip (1).mp4", "my clip (1).mp4"},
		{"identity unicode", "日本語.mp4", "日本語.mp4"},
		// character-level filtering
		{"path separators", "a/b\\c.mp4", "a_b_c.mp4"},
		{"control bytes", "a\x00\x1fb.mp4", "a__b.mp4"},
		{"windows forbidden set", "a<b>c:d\"e|f?g*h", "a_b_c_d_e_f_g_h"},
		// trailing dots and spaces: Windows strips them on create, which
		// would desync the predicted name from the disk name
		{"trailing dots and spaces", "name.mp4..  ", "name.mp4"},
		{"trailing dot only", "v1.", "v1"},
		// nothing usable
		{"all trailing garbage", "...  ", ""},
		{"empty", "", ""},
		{"separators are content, not garbage", "///", "___"},
	}
	for _, tc := range cases {
		if got := SanitizeFileName(tc.in); got != tc.want {
			t.Errorf("%s: SanitizeFileName(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestUUIDName(t *testing.T) {
	for _, ext := range []string{"", "jpg"} {
		name := UUIDName(ext)
		wantLen := 32
		if ext != "" {
			wantLen = 32 + 1 + len(ext)
		}
		if len(name) != wantLen {
			t.Fatalf("UUIDName(%q) = %q, want %d chars", ext, name, wantLen)
		}
		if ext != "" && !strings.HasSuffix(name, "."+ext) {
			t.Errorf("UUIDName(%q) = %q, want the extension appended", ext, name)
		}

		hexPart := strings.TrimSuffix(name, "."+ext)
		if hexPart != strings.ToLower(hexPart) {
			t.Errorf("UUID %q must be lowercase hex", hexPart)
		}
		b, err := hex.DecodeString(hexPart)
		if err != nil || len(b) != 16 {
			t.Fatalf("UUID %q: decode = %v; want 16 decodable bytes", hexPart, err)
		}
		if v := b[6] >> 4; v != 4 {
			t.Errorf("UUID %q version nibble = %x, want 4 (RFC 4122 v4)", hexPart, v)
		}
		if v := b[8] >> 6; v != 2 {
			t.Errorf("UUID %q variant bits = %#b, want 10x1", hexPart, b[8]>>6)
		}
	}
	if UUIDName("") == UUIDName("") {
		t.Error("two drawn UUIDs collided; the random source is not random")
	}
}

func TestDiskNameFor(t *testing.T) {
	// a sane name needs no change: "" is the stored "identical" default
	if got := diskNameFor("clip.mp4"); got != "" {
		t.Errorf("sane name: diskNameFor = %q, want \"\"", got)
	}
	if got := diskNameFor("we/ird:name.mp4"); got != "we_ird_name.mp4" {
		t.Errorf("changed name: diskNameFor = %q, want we_ird_name.mp4", got)
	}
	// garbage-only and nameless both fall back to a bare UUID: no usable
	// text survives, and (with no truncation to blame) no extension does
	// either — the only names that empty out are dots and spaces
	for _, in := range []string{"...", "", " .. . "} {
		got := diskNameFor(in)
		if len(got) != 32 {
			t.Errorf("diskNameFor(%q) = %q, want a bare 32-hex UUID", in, got)
		}
		if _, err := hex.DecodeString(got); err != nil {
			t.Errorf("diskNameFor(%q) = %q: not hex (%v)", in, got, err)
		}
	}
}

func TestLongPathSuffix(t *testing.T) {
	// ASCII: one byte per UTF-16 unit; the limit is inclusive
	if got := longPathSuffix(strings.Repeat("d", 260)); got != "" {
		t.Errorf("path of exactly 260 units: suffix = %q, want none", got)
	}
	if got := longPathSuffix(strings.Repeat("d", 261)); got == "" {
		t.Error("path past 260 units: want the root-too-long hint")
	}
	// MAX_PATH counts UTF-16 code units, not bytes: a CJK path of 300 bytes
	// is only 100 units — legal, and must not fire the hint
	cjk := strings.Repeat("語", 100)
	if len(cjk) != 300 {
		t.Fatalf("cjk fixture = %d bytes, want 300", len(cjk))
	}
	if got := longPathSuffix(cjk); got != "" {
		t.Errorf("path of %d bytes but 100 UTF-16 units: suffix = %q, want none", len(cjk), got)
	}
}
