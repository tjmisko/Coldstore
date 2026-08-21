package thumbnail

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// waitDelay bounds how long Wait blocks after the process has been killed.
// Without it a killed tool that left a child holding the output pipe open
// would hang the worker anyway, which is the very failure the timeout exists
// to prevent.
const waitDelay = 2 * time.Second

// defaultToolTimeout bounds a single invocation of an external renderer. A
// truncated or malformed input can make one sit forever; without a deadline
// one such file stalls the whole thumbnail backlog.
const defaultToolTimeout = 30 * time.Second

// tool is one external binary the generator shells out to.
//
// Both renderers get the identical treatment, because both failure modes are
// identical: a hung process, a process that spawned a child, and a process
// killed by the deadline that has to say WHY in the recorded thumb_error. This
// was written once for ffmpeg and is shared rather than copied, so the PDF
// renderer cannot quietly acquire a weaker version of it.
type tool struct {
	// name appears in error messages, so a recorded thumb_error says which
	// binary gave up.
	name    string
	bin     string
	timeout time.Duration
}

func newTool(name, bin string) tool {
	return tool{name: name, bin: bin, timeout: defaultToolTimeout}
}

// set overrides the binary and per-invocation timeout. A non-positive timeout
// restores the default.
func (t *tool) set(binary string, timeout time.Duration) {
	if binary != "" {
		t.bin = binary
	}
	if timeout > 0 {
		t.timeout = timeout
		return
	}
	t.timeout = defaultToolTimeout
}

// command builds a cancellable invocation. The process gets its own process
// group so cancellation kills any child it spawned, not just the process
// itself.
func (t tool) command(cctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(cctx, t.bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay
	return cmd
}

// deadline derives the context for one invocation.
func (t tool) deadline(ctx context.Context) (context.Context, context.CancelFunc, time.Duration) {
	timeout := t.timeout
	if timeout <= 0 {
		timeout = defaultToolTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	return cctx, cancel, timeout
}

// maxCapturedStderr bounds how much of a tool's diagnostics is kept. The text
// ends up in files.thumb_error, which the UI renders: a renderer that prints a
// warning per malformed object in a 200 MB PDF must not be able to put
// megabytes of it into the catalog.
const maxCapturedStderr = 4096

// limitedBuffer is an io.Writer that keeps at most maxCapturedStderr bytes and
// silently discards the rest.
type limitedBuffer struct {
	buf      []byte
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxCapturedStderr - len(b.buf); room > 0 {
		if len(p) <= room {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:room]...)
			b.overflow = true
		}
	} else if len(p) > 0 {
		b.overflow = true
	}
	// Always report a full write: a short write makes exec close the pipe and
	// the tool die of SIGPIPE, which would turn a chatty renderer into a
	// failed one.
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	s := strings.TrimSpace(string(b.buf))
	if b.overflow {
		return s + " [truncated]"
	}
	return s
}

// fail turns a killed-by-deadline exec failure into a legible timeout error,
// so the recorded thumb_error says why the file was given up on.
func (t tool) fail(cctx context.Context, timeout time.Duration, src string, err error, output string) error {
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s timed out after %s on %s", t.name, timeout, src)
	}
	if errors.Is(cctx.Err(), context.Canceled) {
		return fmt.Errorf("%s cancelled on %s: %w", t.name, src, context.Canceled)
	}
	if output == "" {
		return fmt.Errorf("%s: %w", t.name, err)
	}
	return fmt.Errorf("%s: %w: %s", t.name, err, output)
}
