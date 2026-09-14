package archive

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name string, size int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyMatch(t *testing.T) {
	dir := t.TempDir()
	// dlTemplate-style name: <msgID>_<name>, inside the dialog's media dir
	writeFile(t, dir, "42_clip.mp4", 500)

	vr, err := verify(dir, 42, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !vr.matched {
		t.Fatal("expected match")
	}
	if vr.actualSize != 500 {
		t.Errorf("actualSize = %d, want 500", vr.actualSize)
	}
}

func TestVerifySizeMismatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "42_clip.mp4", 499)

	vr, err := verify(dir, 42, 500)
	if err != nil {
		t.Fatal(err)
	}
	if vr.matched {
		t.Error("expected no match on wrong size")
	}
	if vr.actualSize != 499 || vr.path == "" {
		t.Errorf("expected located file for reporting, got %+v", vr)
	}
}

func TestVerifyIgnoresTmp(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "42_clip.mp4.tmp", 500)

	vr, err := verify(dir, 42, 500)
	if err != nil {
		t.Fatal(err)
	}
	if vr.matched || vr.path != "" {
		t.Errorf(".tmp must be ignored, got %+v", vr)
	}
}

func TestVerifyPrefixIsExact(t *testing.T) {
	dir := t.TempDir()
	// msg 10 must not satisfy a lookup for msg 1
	writeFile(t, dir, "10_other.mp4", 500)

	vr, err := verify(dir, 1, 500)
	if err != nil {
		t.Fatal(err)
	}
	if vr.matched || vr.path != "" {
		t.Errorf("msg 1 should not match msg 10's file, got %+v", vr)
	}
}

func TestVerifyMissing(t *testing.T) {
	dir := t.TempDir()
	vr, err := verify(dir, 42, 500)
	if err != nil {
		t.Fatal(err)
	}
	if vr.matched || vr.path != "" || vr.actualSize != 0 {
		t.Errorf("expected empty result, got %+v", vr)
	}
}
