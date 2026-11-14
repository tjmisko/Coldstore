package journal

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const lockHelperEnv = "JOURNAL_LOCK_HELPER_DIR"

// TestLockHelperProcess is the child half of the cross-process lock tests. It
// does nothing unless the parent set the environment variable.
func TestLockHelperProcess(t *testing.T) {
	dir := os.Getenv(lockHelperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	lock, err := AcquireLock(dir)
	if err != nil {
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(2)
	}
	os.Stdout.WriteString("locked\n")
	// Hold the lock until the parent closes our stdin (or kills us).
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
	_ = lock.Release()
	os.Exit(0)
}

type lockHolder struct {
	cmd   *exec.Cmd
	stdin interface{ Close() error }
}

func startLockHolder(t *testing.T, dir string) *lockHolder {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("helper did not take the lock: %q %v", line, err)
	}
	return &lockHolder{cmd: cmd, stdin: stdin}
}

func TestLock(t *testing.T) {
	t.Run("should return ErrLocked when another process holds the lock", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "journal")
		holder := startLockHolder(t, dir)
		if _, err := AcquireLock(dir); !errors.Is(err, ErrLocked) {
			t.Fatalf("got %v, want ErrLocked", err)
		}
		// Releasing lets it succeed.
		_ = holder.stdin.Close()
		if err := holder.cmd.Wait(); err != nil {
			t.Fatalf("helper exit: %v", err)
		}
		lock, err := AcquireLock(dir)
		if err != nil {
			t.Fatalf("after release: %v", err)
		}
		_ = lock.Release()
	})
	t.Run("should acquire the lock when the previous holder was killed", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "journal")
		holder := startLockHolder(t, dir)
		if err := holder.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = holder.cmd.Wait()
		lock, err := AcquireLock(dir)
		if err != nil {
			t.Fatalf("after kill: %v", err)
		}
		_ = lock.Release()
	})
	t.Run("should return ErrLocked when the same process asks twice", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "journal")
		first, err := AcquireLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := AcquireLock(dir); !errors.Is(err, ErrLocked) {
			t.Fatalf("got %v", err)
		}
		if err := first.Release(); err != nil {
			t.Fatal(err)
		}
		if err := first.Release(); err != nil {
			t.Fatalf("second Release: %v", err)
		}
		again, err := AcquireLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		_ = again.Release()
	})
	t.Run("should create LOCK with mode 0600 holding the pid when acquired", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "journal")
		lock, err := AcquireLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Release()
		info, err := os.Stat(filepath.Join(dir, "LOCK"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("LOCK: %v %v", info, err)
		}
		pid, _ := os.ReadFile(filepath.Join(dir, "LOCK"))
		if strings.TrimSpace(string(pid)) != strconv.Itoa(os.Getpid()) {
			t.Fatalf("LOCK contains %q", pid)
		}
	})
	t.Run("should not disturb the holder's pid when a second acquire fails", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "journal")
		lock, err := AcquireLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Release()
		_, _ = AcquireLock(dir)
		pid, _ := os.ReadFile(filepath.Join(dir, "LOCK"))
		if strings.TrimSpace(string(pid)) != strconv.Itoa(os.Getpid()) {
			t.Fatalf("LOCK contains %q", pid)
		}
	})
}
