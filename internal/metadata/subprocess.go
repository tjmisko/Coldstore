package metadata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	processWaitDelay  = 2 * time.Second
	maxToolDiagnostic = 4096
)

type limitedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	room := maxToolDiagnostic - b.Len()
	if room > 0 {
		kept := value
		if len(kept) > room {
			kept = kept[:room]
			b.overflow = true
		}
		_, _ = b.Buffer.Write(kept)
	} else if len(value) > 0 {
		b.overflow = true
	}
	return len(value), nil
}

func (b *limitedBuffer) diagnostic() string {
	value := strings.TrimSpace(b.String())
	if b.overflow {
		return value + " [truncated]"
	}
	return value
}

func commandContext(ctx context.Context, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = processWaitDelay
	return cmd
}

func runTool(ctx context.Context, binary string, timeout time.Duration, args ...string) ([]byte, error) {
	if timeout <= 0 {
		timeout = defaultToolTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := commandContext(cctx, binary, args...)
	var stdout bytes.Buffer
	var stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return nil, fmt.Errorf("metadata: %s caller deadline exceeded: %w", binary, context.DeadlineExceeded)
		}
		return nil, fmt.Errorf("metadata: %s cancelled: %w", binary, context.Canceled)
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("metadata: %s timed out after %s: %w", binary, timeout, context.DeadlineExceeded)
	}
	if errors.Is(cctx.Err(), context.Canceled) {
		return nil, fmt.Errorf("metadata: %s cancelled: %w", binary, context.Canceled)
	}
	if diagnostic := stderr.diagnostic(); diagnostic != "" {
		return nil, fmt.Errorf("metadata: %s: %w: %s", binary, err, diagnostic)
	}
	return nil, fmt.Errorf("metadata: %s: %w", binary, err)
}
