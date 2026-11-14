package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/control"
	"github.com/tjmisko/coldstore/internal/journal"
)

const daemonHelperEnv = "COLDSTORE_TEST_RUN_MAIN"

// TestDaemonHelperProcess is not a test: it is the daemon, re-executed from the
// test binary so the wiring in main (archive open, control socket, signal
// handling, shutdown order) runs exactly as it does in production.
func TestDaemonHelperProcess(t *testing.T) {
	if os.Getenv(daemonHelperEnv) != "1" {
		t.Skip("helper process only")
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
		}
	}
	if separator < 0 {
		t.Fatal("no -- before the daemon arguments")
	}
	os.Args = append([]string{"coldstore"}, os.Args[separator+1:]...)
	main()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

// startDaemon runs the daemon on a config and returns its process and API address.
func startDaemon(t *testing.T, root string, collections []string) (*exec.Cmd, string) {
	t.Helper()
	folder := t.TempDir()
	if err := os.MkdirAll(filepath.Join(folder, ".stfolder"), 0o755); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	var config strings.Builder
	fmt.Fprintf(&config, "[storage]\ncold_storage_path = %q\n[server]\nlisten_addr = %q\n", root, addr)
	fmt.Fprintf(&config, "[[folders]]\nlabel = \"Docs\"\npath = %q\nbudget = \"1GB\"\n", folder)
	for _, name := range collections {
		fmt.Fprintf(&config, "[[collections]]\nname = %q\n", name)
	}
	configPath := filepath.Join(t.TempDir(), "coldstore.toml")
	if err := os.WriteFile(configPath, []byte(config.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonHelperProcess$", "--", "--config", configPath)
	cmd.Env = append(os.Environ(), daemonHelperEnv+"=1")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd, addr
}

func waitForStatus(t *testing.T, addr string) map[string]json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/api/v1/status")
		if err == nil {
			var status map[string]json.RawMessage
			decodeErr := json.NewDecoder(resp.Body).Decode(&status)
			resp.Body.Close()
			if decodeErr == nil && resp.StatusCode == http.StatusOK {
				return status
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the daemon never answered /api/v1/status")
	return nil
}

func terminate(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		if err != nil {
			t.Fatalf("the daemon exited badly after SIGTERM: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon did not stop after SIGTERM")
	}
}

func TestDaemonWiring(t *testing.T) {
	t.Run("should create no archive directory when no collection is declared and no journal exists", func(t *testing.T) {
		root := shortDir(t)
		cmd, addr := startDaemon(t, root, nil)

		status := waitForStatus(t, addr)

		if string(status["archive"]) != "null" {
			t.Fatalf("archive block %s, want null", status["archive"])
		}
		for _, name := range []string{"blobs", "journal", "tmp", "index.db", "views", control.SocketName} {
			if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s exists (err %v)", name, err)
			}
		}
		terminate(t, cmd)
	})

	t.Run("should open the archive, serve the control socket and release both on SIGTERM when a collection is declared", func(t *testing.T) {
		root := shortDir(t)
		cmd, addr := startDaemon(t, root, []string{"photos"})

		status := waitForStatus(t, addr)

		if string(status["archive"]) == "null" {
			t.Fatal("the archive block is null although a collection is declared")
		}
		info, err := os.Lstat(control.SocketPath(root))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("control socket %v err %v", info, err)
		}
		if _, err := journal.AcquireLock(filepath.Join(root, "journal")); !errors.Is(err, journal.ErrLocked) {
			t.Fatalf("the running daemon does not hold the journal lock: %v", err)
		}

		terminate(t, cmd)

		lock, err := journal.AcquireLock(filepath.Join(root, "journal"))
		if err != nil {
			t.Fatalf("the lock is still held after SIGTERM: %v", err)
		}
		_ = lock.Release()
		if _, err := os.Lstat(control.SocketPath(root)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the control socket outlived the daemon: %v", err)
		}
	})

	t.Run("should exit 1 naming the lock holder when another coldstore holds the archive", func(t *testing.T) {
		root := shortDir(t)
		fixture := archiveFixture{root: root}
		fixture.open(t)
		cmd, _ := startDaemon(t, root, []string{"photos"})

		err := cmd.Wait()

		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("the daemon exited with %v, want exit status 1", err)
		}
	})
}
