package journal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

// Lock is the single-writer lock: flock(LOCK_EX|LOCK_NB) on journal/LOCK. The
// kernel drops it when the holder dies, so a killed process never leaves a
// stale lock behind. It is deliberately not routed through fsx: it is not a
// durability call.
type Lock struct {
	mu   sync.Mutex
	file *os.File
}

// AcquireLock takes the lock, creating dir and LOCK (0600) if absent. It
// returns ErrLocked when another process, or another Lock, holds it.
func AcquireLock(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("journal: create lock directory: %w", err)
	}
	path := filepath.Join(dir, "LOCK")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: open lock file: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("journal: lock %s: %w", path, err)
	}
	// The pid is for humans reading the file; failing to write it is harmless.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{file: f}, nil
}

// Release drops the lock. It is safe to call more than once.
func (l *Lock) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	unlockErr := unix.Flock(int(f.Fd()), unix.LOCK_UN)
	closeErr := f.Close()
	if unlockErr != nil {
		return fmt.Errorf("journal: unlock: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("journal: close lock file: %w", closeErr)
	}
	return nil
}
