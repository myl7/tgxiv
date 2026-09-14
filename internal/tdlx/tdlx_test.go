package tdlx

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRunCapturesStdout guards against the typed-nil interface trap: if the
// Stdout field were *os.File, a nil value would still make cmd.Stdout a non-nil
// io.Writer and exec would silently drop the child's output. With io.Writer, a
// real writer must capture it.
func TestRunCapturesStdout(t *testing.T) {
	var buf bytes.Buffer
	r := &Runner{Bin: "/bin/echo", Namespace: "default", Stdout: &buf}

	if err := r.run(context.Background(), []string{"hello-tgxiv"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	// run prepends "-n default", so echo prints: -n default hello-tgxiv
	if got := buf.String(); !strings.Contains(got, "hello-tgxiv") {
		t.Fatalf("stdout not captured, got %q", got)
	}
}

// writeTdl drops a minimal executable tdl at path. The content never runs in
// these tests; only the exec bit and the name matter to resolveBin.
func writeTdl(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestResolveBinVerbatim checks that any Bin other than the bare default "tdl"
// is used as-is, without touching the filesystem (so nonexistent paths survive
// until exec time, where they produce exec's own actionable error).
func TestResolveBinVerbatim(t *testing.T) {
	for _, bin := range []string{"./no-such-dir/tdl", "/no/such/tdl", "mytdl"} {
		got, err := resolveBin(bin)
		if err != nil {
			t.Fatalf("resolveBin(%q): %v", bin, err)
		}
		if got != bin {
			t.Fatalf("resolveBin(%q) = %q, want verbatim", bin, got)
		}
	}
}

// TestResolveBinPathHit checks the default "tdl" resolving via PATH to the
// absolute path of the executable.
func TestResolveBinPathHit(t *testing.T) {
	dir := t.TempDir()
	writeTdl(t, filepath.Join(dir, "tdl"))
	t.Setenv("PATH", dir)

	got, err := resolveBin("tdl")
	if err != nil {
		t.Fatalf("resolveBin: %v", err)
	}
	if want := filepath.Join(dir, "tdl"); got != want {
		t.Fatalf("resolveBin = %q, want %q", got, want)
	}
}

// TestResolveBinCwdFallback checks the default "tdl" falling back to the
// working directory when PATH has no tdl, resolving to the absolute path.
func TestResolveBinCwdFallback(t *testing.T) {
	dir := t.TempDir()
	writeTdl(t, filepath.Join(dir, "tdl"))
	t.Setenv("PATH", t.TempDir()) // empty: no tdl anywhere on PATH
	t.Chdir(dir)

	got, err := resolveBin("tdl")
	if err != nil {
		t.Fatalf("resolveBin: %v", err)
	}
	if want := filepath.Join(dir, "tdl"); got != want {
		t.Fatalf("resolveBin = %q, want %q", got, want)
	}
}

// TestResolveBinNotFound checks the actionable error when tdl is neither on
// PATH nor in the working directory.
func TestResolveBinNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Chdir(t.TempDir())

	_, err := resolveBin("tdl")
	if err == nil {
		t.Fatal("resolveBin succeeded without any tdl present")
	}
	if !strings.Contains(err.Error(), "--tdl") {
		t.Fatalf("error lacks --tdl remedy: %v", err)
	}
}

// TestResolveBinPathBeatsCwd pins the priority: with an executable tdl both on
// PATH and in the working directory, the PATH copy wins.
func TestResolveBinPathBeatsCwd(t *testing.T) {
	cwdDir := t.TempDir()
	writeTdl(t, filepath.Join(cwdDir, "tdl"))
	pathDir := t.TempDir()
	writeTdl(t, filepath.Join(pathDir, "tdl"))

	t.Setenv("PATH", pathDir)
	t.Chdir(cwdDir)

	got, err := resolveBin("tdl")
	if err != nil {
		t.Fatalf("resolveBin: %v", err)
	}
	if want := filepath.Join(pathDir, "tdl"); got != want {
		t.Fatalf("resolveBin = %q, want PATH copy %q", got, want)
	}
}

// TestResolveBinCwdNotExecutable checks that a plain data file named tdl in
// the working directory is not accepted. Windows has no exec bit, so 0644 is
// as runnable as anything there; the LookPath probe is still correct there.
func TestResolveBinCwdNotExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no exec bit on windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tdl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Chdir(dir)

	if got, err := resolveBin("tdl"); err == nil {
		t.Fatalf("non-executable cwd tdl accepted: %q", got)
	}
}

// TestRunnerCheckResolvesCwdBin wires the resolution into the Runner: Check
// must find the working-directory copy through the memoized helper.
func TestRunnerCheckResolvesCwdBin(t *testing.T) {
	dir := t.TempDir()
	writeTdl(t, filepath.Join(dir, "tdl"))
	t.Setenv("PATH", t.TempDir())
	t.Chdir(dir)

	r := &Runner{Bin: "tdl", Namespace: "default"}
	if err := r.Check(context.Background()); err != nil {
		t.Fatalf("Check via cwd-resolved tdl: %v", err)
	}
}
