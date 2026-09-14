package tdlx

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
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

// writeTdlScript drops an executable fake tdl at path running the given shell
// body. Unlike writeTdl's inert stub, this body does run: it prints canned
// stdout (and records its arguments) so ChatList goes through the real exec
// path.
func writeTdlScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// readScriptArgs reads back one argument per line, as recorded by a fake tdl's
// 'printf %s "$@"' redirection, for exact-invocation asserts.
func readScriptArgs(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// chatLsJSON is a canned "chat ls -o json" array covering every username shape
// tdl emits: a real one, the "-" fallback for basic groups, and the key absent
// entirely (omitempty).
const chatLsJSON = `[` +
	`{"id":3992083278,"type":"channel","visible_name":"Some Channel","username":"somechannel"},` +
	`{"id":42,"type":"group","visible_name":"Grp","username":"-"},` +
	`{"id":7,"type":"private","visible_name":""}` +
	`]`

// TestChatListParsesDialogs checks the happy path end-to-end: the exact
// invocation (namespace flag, -o json, -f only when a filter is set) and the
// parsed, normalized DialogInfo values.
func TestChatListParsesDialogs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh script fake tdl")
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	writeTdlScript(t, filepath.Join(dir, "tdl"),
		"printf '%s\\n' \"$@\" > \""+argsPath+"\"\n"+
			"cat <<'JSON'\n"+chatLsJSON+"\nJSON\n")

	r := &Runner{Bin: filepath.Join(dir, "tdl"), Namespace: "ns"}

	ds, err := r.ChatList(context.Background(), ChatListOptions{})
	if err != nil {
		t.Fatalf("ChatList: %v", err)
	}
	want := []DialogInfo{
		{ID: 3992083278, Type: "channel", Title: "Some Channel", Username: "somechannel"},
		{ID: 42, Type: "group", Title: "Grp", Username: ""}, // "-" normalized away
		{ID: 7, Type: "private", Title: "", Username: ""},   // key absent stays ""
	}
	if !reflect.DeepEqual(ds, want) {
		t.Fatalf("ChatList = %#v, want %#v", ds, want)
	}
	if got, want := readScriptArgs(t, argsPath), []string{"-n", "ns", "chat", "ls", "-o", "json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("invocation without filter = %q, want %q", got, want)
	}

	if _, err := r.ChatList(context.Background(), ChatListOptions{Filter: "ID == 3992083278"}); err != nil {
		t.Fatalf("ChatList with filter: %v", err)
	}
	wantArgs := []string{"-n", "ns", "chat", "ls", "-o", "json", "-f", "ID == 3992083278"}
	if got := readScriptArgs(t, argsPath); !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("invocation with filter = %q, want %q", got, wantArgs)
	}
}

// TestChatListEmpty checks that an empty dialog list is a nil slice, not an
// error: "no dialogs" is a normal outcome of chat ls.
func TestChatListEmpty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh script fake tdl")
	}
	dir := t.TempDir()
	writeTdlScript(t, filepath.Join(dir, "tdl"), "echo '[]'\n")

	ds, err := (&Runner{Bin: filepath.Join(dir, "tdl"), Namespace: "ns"}).ChatList(context.Background(), ChatListOptions{})
	if err != nil {
		t.Fatalf("ChatList: %v", err)
	}
	if ds != nil {
		t.Fatalf("ChatList = %#v, want nil", ds)
	}
}

// TestChatListFailureNamesInvocation checks that a failing tdl surfaces as an
// error naming the invocation, mirroring run()'s "tdl %v: %w" shape.
func TestChatListFailureNamesInvocation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh script fake tdl")
	}
	dir := t.TempDir()
	writeTdlScript(t, filepath.Join(dir, "tdl"), "exit 3\n")

	_, err := (&Runner{Bin: filepath.Join(dir, "tdl"), Namespace: "ns"}).ChatList(context.Background(), ChatListOptions{})
	if err == nil {
		t.Fatal("ChatList succeeded despite tdl failing")
	}
	if !strings.Contains(err.Error(), "tdl") || !strings.Contains(err.Error(), "chat ls") {
		t.Fatalf("error does not name the invocation: %v", err)
	}
}

// TestChatListMalformedOutput checks the parse error: it must say the output
// could not be parsed, and must NOT quote the output — dialog titles are
// private data and have no business in logs.
func TestChatListMalformedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh script fake tdl")
	}
	dir := t.TempDir()
	writeTdlScript(t, filepath.Join(dir, "tdl"), "echo 'TDLX-NOT-JSON'\n")

	_, err := (&Runner{Bin: filepath.Join(dir, "tdl"), Namespace: "ns"}).ChatList(context.Background(), ChatListOptions{})
	if err == nil {
		t.Fatal("ChatList succeeded despite unparseable stdout")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("error does not mention the parse failure: %v", err)
	}
	if strings.Contains(err.Error(), "TDLX-NOT-JSON") {
		t.Fatalf("error leaks raw tdl output: %v", err)
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
