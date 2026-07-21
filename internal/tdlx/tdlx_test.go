package tdlx

import (
	"bytes"
	"context"
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

	if err := r.run(context.Background(), []string{"hello-tgca"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	// run prepends "-n default", so echo prints: -n default hello-tgca
	if got := buf.String(); !strings.Contains(got, "hello-tgca") {
		t.Fatalf("stdout not captured, got %q", got)
	}
}
