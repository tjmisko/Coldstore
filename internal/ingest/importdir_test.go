package ingest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type skipLog struct {
	mu      sync.Mutex
	entries map[string]string
}

func (s *skipLog) record(path, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]string{}
	}
	s.entries[path] = reason
}

func collect(t *testing.T, root string, skips *skipLog) []Arrival {
	t.Helper()
	var out []Arrival
	for a := range ImportDir(root, "photos", skips.record).Arrivals(context.Background()) {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestImportDir(t *testing.T) {
	t.Run("should emit one arrival per regular file when walking a tree", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "a.txt"), "aaa")
		writeFile(t, filepath.Join(root, "sub", "deep", "b.txt"), "bb")
		skips := &skipLog{}
		arrivals := collect(t, root, skips)
		if len(arrivals) != 2 || arrivals[0].Path != "a.txt" || arrivals[1].Path != "sub/deep/b.txt" {
			t.Fatalf("arrivals = %+v", arrivals)
		}
		if arrivals[0].Folder != "photos" {
			t.Fatalf("folder = %q", arrivals[0].Folder)
		}
		rc, err := arrivals[1].Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		body, err := io.ReadAll(rc)
		if err != nil || string(body) != "bb" {
			t.Fatalf("body %q err %v", body, err)
		}
		if len(skips.entries) != 0 {
			t.Fatalf("skipped %v", skips.entries)
		}
	})

	t.Run("should report and not open a symlink", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "real.txt"), "x")
		link := filepath.Join(root, "link.txt")
		if err := os.Symlink(filepath.Join(root, "real.txt"), link); err != nil {
			t.Fatal(err)
		}
		skips := &skipLog{}
		arrivals := collect(t, root, skips)
		if len(arrivals) != 1 || arrivals[0].Path != "real.txt" {
			t.Fatalf("arrivals = %+v", arrivals)
		}
		if skips.entries[link] != "symlink" {
			t.Fatalf("skips = %v", skips.entries)
		}
	})

	t.Run("should report and not open a FIFO", func(t *testing.T) {
		root := t.TempDir()
		fifo := filepath.Join(root, "pipe")
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		skips := &skipLog{}
		done := make(chan []Arrival, 1)
		go func() { done <- collect(t, root, skips) }()
		select {
		case arrivals := <-done:
			if len(arrivals) != 0 || skips.entries[fifo] != "fifo" {
				t.Fatalf("arrivals %+v skips %v", arrivals, skips.entries)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("walk blocked on a FIFO")
		}
	})

	t.Run("should report a file whose name is not valid UTF-8", func(t *testing.T) {
		root := t.TempDir()
		bad := filepath.Join(root, "bad\xffname.txt")
		if err := os.WriteFile(bad, []byte("x"), 0o644); err != nil {
			t.Skipf("filesystem refuses the name: %v", err)
		}
		writeFile(t, filepath.Join(root, "good.txt"), "y")
		skips := &skipLog{}
		arrivals := collect(t, root, skips)
		if len(arrivals) != 1 || arrivals[0].Path != "good.txt" {
			t.Fatalf("arrivals = %+v", arrivals)
		}
		if !strings.Contains(skips.entries[bad], "UTF-8") {
			t.Fatalf("skips = %v", skips.entries)
		}
	})

	t.Run("should continue past an unreadable file and report it", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root reads everything")
		}
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "a-locked", "inside.txt"), "x")
		writeFile(t, filepath.Join(root, "b-open.txt"), "y")
		writeFile(t, filepath.Join(root, "c-secret.txt"), "z")
		if err := os.Chmod(filepath.Join(root, "a-locked"), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "a-locked"), 0o755) })
		if err := os.Chmod(filepath.Join(root, "c-secret.txt"), 0o000); err != nil {
			t.Fatal(err)
		}
		skips := &skipLog{}
		arrivals := collect(t, root, skips)
		if len(arrivals) != 2 {
			t.Fatalf("arrivals = %+v", arrivals)
		}
		if !strings.HasPrefix(skips.entries[filepath.Join(root, "a-locked")], "unreadable") {
			t.Fatalf("skips = %v", skips.entries)
		}
		if _, err := arrivals[1].Open(); err == nil {
			t.Fatal("Open succeeded on an unreadable file")
		}
	})

	t.Run("should use the file mtime as the arrival mtime", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "a.txt")
		writeFile(t, path, "x")
		want := time.Date(2020, 5, 6, 7, 8, 9, 0, time.UTC)
		if err := os.Chtimes(path, want, want); err != nil {
			t.Fatal(err)
		}
		arrivals := collect(t, root, &skipLog{})
		if len(arrivals) != 1 || !arrivals[0].Mtime.Equal(want) {
			t.Fatalf("arrivals = %+v", arrivals)
		}
	})

	t.Run("should stop walking when the context is cancelled", func(t *testing.T) {
		root := t.TempDir()
		for _, name := range []string{"a", "b", "c"} {
			writeFile(t, filepath.Join(root, name), name)
		}
		ctx, cancel := context.WithCancel(context.Background())
		ch := ImportDir(root, "photos", nil).Arrivals(ctx)
		<-ch
		cancel()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case _, open := <-ch:
				if !open {
					return
				}
			case <-deadline:
				t.Fatal("channel never closed")
			}
		}
	})

	t.Run("should fail the read instead of reporting EOF when the file grows while open", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "a.txt")
		writeFile(t, path, "abc")
		arrivals := collect(t, root, &skipLog{})
		rc, err := arrivals[0].Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("more"); err != nil {
			t.Fatal(err)
		}
		f.Close()
		_, err = io.ReadAll(rc)
		if !errors.Is(err, ErrChangedWhileReading) {
			t.Fatalf("err = %v", err)
		}
	})
}
