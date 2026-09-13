// Package tdlx runs the external tdl binary. The archiver drives tdl as a black
// box: it prepares inputs (a channel, a batch JSON) and lets tdl do the login,
// peer resolution, and actual transfer. Verification is done separately by the
// caller against the filesystem, so this package only cares about launching tdl
// and propagating success/failure and interruption.
package tdlx

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// Runner launches a specific tdl binary under a fixed session namespace.
type Runner struct {
	// Bin is the tdl executable (a path, or a name resolved on PATH).
	Bin string
	// Namespace is tdl's -n session namespace. Export and download must share
	// it so the channel's peer cache is available to both.
	Namespace string
	// Stdout/Stderr receive tdl's output. Nil means os.Stdout/os.Stderr. The
	// type is io.Writer, not *os.File, so a nil field is a true nil interface
	// (a nil *os.File would be a non-nil interface wrapping a nil pointer, which
	// exec would turn into an invalid child fd and silently drop tdl's output).
	Stdout, Stderr io.Writer
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

func (r *Runner) run(ctx context.Context, args []string) error {
	// namespace is a persistent flag, valid before the subcommand
	full := append([]string{"-n", r.Namespace}, args...)

	// CommandContext is required because we set a custom cmd.Cancel below; our
	// Cancel replaces the default (which would SIGKILL) so tdl gets SIGINT.
	cmd := exec.CommandContext(ctx, r.Bin, full...)

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
	cmd := exec.CommandContext(ctx, r.Bin, "version")
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tdl binary %q not runnable: %w", r.Bin, err)
	}
	return nil
}
