// Package tdlx runs the external tdl binary. The archiver drives tdl as a black
// box: it prepares inputs (a channel, a batch JSON) and lets tdl do the login,
// peer resolution, and actual transfer. Verification is done separately by the
// caller against the filesystem, so this package only cares about launching tdl
// and propagating success/failure and interruption.
package tdlx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Runner launches a specific tdl binary under a fixed session namespace.
type Runner struct {
	// Bin is the tdl executable: a path, or a name resolved on PATH. The bare
	// default "tdl" additionally falls back to tdl/tdl.exe in the working
	// directory; see resolveBin.
	Bin string
	// Namespace is tdl's -n session namespace. Export and download must share
	// it so the channel's peer cache is available to both.
	Namespace string
	// Stdout/Stderr receive tdl's output. Nil means os.Stdout/os.Stderr. The
	// type is io.Writer, not *os.File, so a nil field is a true nil interface
	// (a nil *os.File would be a non-nil interface wrapping a nil pointer, which
	// exec would turn into an invalid child fd and silently drop tdl's output).
	Stdout, Stderr io.Writer

	// resolve* memoize the default-bin search: a Runner is reused across the
	// batches of a download run, and resolution must not flip binaries mid-run.
	// They stay zero for an explicit Bin, which needs no probing at all.
	resolveOnce sync.Once
	resolvedBin string
	resolveErr  error
}

// ExportOptions configures "tdl chat export".
type ExportOptions struct {
	Chat    string // -c: channel username, id, or link
	Output  string // -o: destination JSON path
	SinceID int    // when > 0, export only messages with id >= SinceID (incremental)
}

// DownloadOptions configures "tdl dl".
type DownloadOptions struct {
	BatchFile string // -f: the batch JSON listing message ids
	Dir       string // -d: download directory
	Threads   int    // --threads, 0 leaves tdl's default
	Limit     int    // --limit, 0 leaves tdl's default
	KeepOrder bool   // --keep-order: download in batch-file order, not by message id
	// Template is --template, the output filename template (e.g.
	// "{{ .MessageID }}_{{ filenamify .FileName }}"). Empty leaves tdl's own
	// default "<dialogID>_<msgID>_<name>" naming; the per-dialog media layout
	// overrides it because the dialog id moved into the directory.
	Template string
}

// DialogInfo is one entry of "tdl chat ls -o json" output.
type DialogInfo struct {
	ID       int64  // Telegram dialog id, bare positive form
	Type     string // "private" | "channel" | "group" (tdl's designation, verbatim)
	Title    string // visible_name; "" when the dialog has none
	Username string // WITHOUT '@'; "" when absent (tdl emits "-" or omits the key)
}

// ChatListOptions configures "tdl chat ls".
type ChatListOptions struct {
	Filter string // -f expression over the dialog fields (e.g. "ID == 42"); empty lists everything
}

// Export runs "tdl chat export --all --with-content --raw" for one channel.
// With SinceID > 0 it uses "--type id -i <SinceID>", which tdl interprets as
// "messages with id >= SinceID" (verified against the API), fetching only the
// newer tail for an incremental sync.
func (r *Runner) Export(ctx context.Context, o ExportOptions) error {
	args := []string{
		"chat", "export",
		"-c", o.Chat,
		"-o", o.Output,
		"--all", "--with-content", "--raw",
	}
	if o.SinceID > 0 {
		args = append(args, "--type", "id", "-i", strconv.Itoa(o.SinceID))
	}
	return r.run(ctx, args)
}

// Download runs "tdl dl" over one batch file. --skip-same avoids re-fetching
// files already present at the right size; --continue keeps it non-interactive.
func (r *Runner) Download(ctx context.Context, o DownloadOptions) error {
	args := []string{
		"dl",
		"-f", o.BatchFile,
		"-d", o.Dir,
		"--skip-same",
		"--continue",
	}
	if o.KeepOrder {
		args = append(args, "--keep-order")
	}
	if o.Template != "" {
		args = append(args, "--template", o.Template)
	}
	if o.Threads > 0 {
		args = append(args, "--threads", fmt.Sprintf("%d", o.Threads))
	}
	if o.Limit > 0 {
		args = append(args, "--limit", fmt.Sprintf("%d", o.Limit))
	}
	return r.run(ctx, args)
}

// Login runs "tdl login" interactively (QR by default, or -T code).
func (r *Runner) Login(ctx context.Context, code bool) error {
	mode := "qr"
	if code {
		mode = "code"
	}
	return r.run(ctx, []string{"login", "-T", mode})
}

// ChatList runs "tdl chat ls -o json" and parses the dialog list. Callers need
// a dialog's numeric id, type, title, and @username — e.g. to resolve a channel
// link to the id that export and download talk about — and chat ls is tdl's
// only machine-readable view of them. Filter passes tdl a -f expression so a
// lookup fetches exactly one dialog instead of listing every chat of the
// account.
func (r *Runner) ChatList(ctx context.Context, o ChatListOptions) ([]DialogInfo, error) {
	args := []string{"chat", "ls", "-o", "json"}
	if o.Filter != "" {
		args = append(args, "-f", o.Filter)
	}
	out, err := r.runCapture(ctx, args)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		// tdl printed no list at all; "no dialogs" is not an error
		return nil, nil
	}
	var ds []tdlDialog
	// The output is deliberately not embedded in the error: dialog titles are
	// private data and must not leak into logs; the json error already carries
	// the offset of the offending byte.
	if err := json.Unmarshal(out, &ds); err != nil {
		return nil, fmt.Errorf("parse tdl chat ls output as a dialog JSON array: %w", err)
	}
	if len(ds) == 0 {
		return nil, nil
	}
	is := make([]DialogInfo, len(ds))
	for i, d := range ds {
		is[i] = DialogInfo{ID: d.ID, Type: d.Type, Title: d.VisibleName, Username: d.Username}
		if is[i].Username == "-" {
			// tdl's fallback for basic groups, which have no username at all
			is[i].Username = ""
		}
	}
	return is, nil
}

// tdlDialog mirrors one object of tdl's "chat ls -o json" array under tdl's own
// JSON names, so decoding is a plain Unmarshal. username is omitempty on tdl's
// side: a missing key just leaves the field "".
type tdlDialog struct {
	ID          int64  `json:"id"`
	Type        string `json:"type"`
	VisibleName string `json:"visible_name"`
	Username    string `json:"username"`
}

// resolveBin maps a configured Bin to the executable to launch. Only the bare
// default "tdl" is searched (PATH, then the working directory); any other value
// from --tdl/TGXIV_TDL is used verbatim so odd setups (relative paths, renames)
// keep working without filesystem probing.
func resolveBin(bin string) (string, error) {
	if bin != "tdl" {
		return bin, nil
	}
	if p, ok := lookPathOK("tdl"); ok {
		return p, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve tdl: %w", err)
	}
	// LookPath (not plain stat) so unix checks the exec bit and Windows PATHEXT
	// finds tdl.exe via the "tdl" probe; the explicit tdl.exe probe covers
	// PATHEXT oddities where LookPath would not append the extension.
	if p, ok := lookPathOK(filepath.Join(cwd, "tdl")); ok {
		return p, nil
	}
	if p, ok := lookPathOK(filepath.Join(cwd, "tdl.exe")); ok {
		return p, nil
	}
	return "", fmt.Errorf("tdl executable not found: not in PATH and no tdl/tdl.exe in the working directory — put tdl in PATH, place tdl(.exe) in the working directory, or point --tdl/TGXIV_TDL at it")
}

// lookPathOK wraps exec.LookPath, treating ErrDot as a hit: Go flags
// cwd-relative results (e.g. a "." PATH entry) with ErrDot, but the file was
// found and is runnable. The returned path is always absolute.
func lookPathOK(name string) (string, bool) {
	p, err := exec.LookPath(name)
	if err != nil && !errors.Is(err, exec.ErrDot) {
		return "", false
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs, true
	}
	return "", false
}

// bin resolves the effective tdl executable for this runner, once per Runner.
func (r *Runner) bin() (string, error) {
	r.resolveOnce.Do(func() {
		r.resolvedBin, r.resolveErr = resolveBin(r.Bin)
	})
	return r.resolvedBin, r.resolveErr
}

func (r *Runner) run(ctx context.Context, args []string) error {
	bin, err := r.bin()
	if err != nil {
		return err
	}
	return startWait(ctx, r.buildCmd(ctx, bin, args), args)
}

// runCapture is run for subcommands whose stdout is data to parse (chat ls -o
// json), not progress chatter: stdout goes to a buffer, overriding Runner.Stdout,
// while stderr keeps flowing to the runner's writer so tdl's zap logs stay
// visible and a failing run can still explain itself.
func (r *Runner) runCapture(ctx context.Context, args []string) ([]byte, error) {
	bin, err := r.bin()
	if err != nil {
		return nil, err
	}
	cmd := r.buildCmd(ctx, bin, args)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := startWait(ctx, cmd, args); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// buildCmd assembles the exec.Cmd every subcommand launches: the -n namespace
// prefix, the inherited standard streams, and the interrupt-then-kill
// cancellation. Sharing one constructor keeps run and runCapture from drifting
// apart on Ctrl-C handling; callers may only override cmd.Stdout (to capture
// it) on the returned cmd.
func (r *Runner) buildCmd(ctx context.Context, bin string, args []string) *exec.Cmd {
	// namespace is a persistent flag, valid before the subcommand
	full := append([]string{"-n", r.Namespace}, args...)

	// CommandContext is required because we set a custom cmd.Cancel below; our
	// Cancel replaces the default (which would SIGKILL) so tdl gets SIGINT.
	cmd := exec.CommandContext(ctx, bin, full...)

	cmd.Stdout = os.Stdout
	if r.Stdout != nil {
		cmd.Stdout = r.Stdout
	}
	cmd.Stderr = os.Stderr
	if r.Stderr != nil {
		cmd.Stderr = r.Stderr
	}
	cmd.Stdin = os.Stdin

	// On ctx cancel (user Ctrl-C), send SIGINT so tdl can flush its resume key
	// and leave clean .tmp files, then give it a grace period before the kill.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(os.Interrupt)
	}
	cmd.WaitDelay = 15 * time.Second
	return cmd
}

// startWait runs a built cmd to completion and shapes failures the one way
// every invocation reports them: start failures say so, a finished context's
// own reason (cancel/deadline) beats the exec error it caused, and anything
// else names the tdl invocation so logs show what was attempted.
func startWait(ctx context.Context, cmd *exec.Cmd, args []string) error {
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start tdl: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		// prefer the reason the context ended (cancel/deadline) when present
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("tdl %v: %w", args, err)
	}
	return nil
}

// Check verifies the configured binary is runnable by invoking "tdl version".
func (r *Runner) Check(ctx context.Context) error {
	bin, err := r.bin()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, "version")
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tdl binary %q not runnable: %w", bin, err)
	}
	return nil
}
