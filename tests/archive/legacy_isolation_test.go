package archivetier

import (
	"context"
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

	"github.com/tjmisko/coldstore/internal/archive"
	"github.com/tjmisko/coldstore/internal/catalog"
)

func writeLegacyTree(t *testing.T) (coldDir, syncDir string) {
	t.Helper()
	base := t.TempDir()
	coldDir = filepath.Join(base, "cold")
	syncDir = filepath.Join(base, "sync", "Photos")
	for _, dir := range []string{coldDir, filepath.Join(syncDir, ".stfolder"), filepath.Join(syncDir, "2025")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"2025/a.jpg": "a legacy photo that coldstore manages\n",
		"2025/b.jpg": "another legacy photo\n",
		"top.txt":    "a note\n",
	} {
		full := filepath.Join(syncDir, filepath.FromSlash(name))
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, sourceMtime, sourceMtime); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := catalog.Open(filepath.Join(coldDir, "catalog.db"))
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	rec := catalog.FileRecord{
		FolderLabel: "Photos", RelativePath: "2025/a.jpg", Filename: "a.jpg", Extension: ".jpg",
		SizeBytes: 38, FileMtime: sourceMtime, FileAtime: sourceMtime, Status: catalog.StatusSynced,
	}
	if _, err := cat.UpsertFile(&rec, 1); err != nil {
		t.Fatalf("UpsertFile: %v", err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	return coldDir, syncDir
}

func legacySnapshot(t *testing.T, coldDir, syncDir string) []string {
	t.Helper()
	snapshot := treeSnapshot(t, syncDir)
	for _, name := range []string{"catalog.db", "catalog.db-wal", "catalog.db-shm"} {
		path := filepath.Join(coldDir, name)
		if _, err := os.Stat(path); err != nil {
			snapshot = append(snapshot, name+"|absent")
			continue
		}
		for _, line := range treeSnapshot(t, coldDir) {
			if strings.HasPrefix(line, name+"|") {
				snapshot = append(snapshot, line)
			}
		}
	}
	return snapshot
}

func TestLegacyIsolation(t *testing.T) {
	t.Run("should leave the sync folder and catalog.db unchanged when the archive is exercised end to end", func(t *testing.T) {
		ctx := context.Background()
		coldDir, syncDir := writeLegacyTree(t)
		before := legacySnapshot(t, coldDir, syncDir)

		opts := serviceOptions(coldDir)
		service := openService(t, opts)
		ts := newAPIServer(t, coldDir, service)

		id, err := service.ImportStart(collectionName, writeSource(t, demoSource()))
		if err != nil {
			t.Fatalf("ImportStart: %v", err)
		}
		if status := waitForImport(t, service, id); status.State != "done" || status.FailedCount != 0 {
			t.Fatalf("import: %+v", status)
		}
		mustPutFile(t, ts.URL, sourceFile{"Uploads/one.txt", "uploaded\n"})
		entry := entryByPath(t, service.Index(), "Letters/a.txt")
		if _, err := service.Tag(ctx, "test", []archive.TagChange{{IDs: []int64{entry.EntryID}, Add: []string{"x/y"}}}); err != nil {
			t.Fatalf("Tag: %v", err)
		}
		if _, err := service.Trash(ctx, "test", []int64{entry.EntryID}, true); err != nil {
			t.Fatalf("Trash: %v", err)
		}
		if report := runScrub(t, service); len(report.Findings) != 0 {
			t.Fatalf("scrub findings on a healthy archive: %+v", report.Findings)
		}
		closeService(t, service)
		if _, err := archive.RebuildIndex(ctx, opts); err != nil {
			t.Fatalf("RebuildIndex: %v", err)
		}

		after := legacySnapshot(t, coldDir, syncDir)
		if strings.Join(before, "\n") != strings.Join(after, "\n") {
			t.Fatalf("legacy state changed\nbefore: %v\nafter:  %v", before, after)
		}
		for _, name := range []string{"archive", "trash"} {
			if _, err := os.Stat(filepath.Join(coldDir, name)); err == nil {
				t.Errorf("the archive core created legacy directory %s/", name)
			}
		}
	})

	t.Run("should create no archive directory when no collection is declared", func(t *testing.T) {
		coldDir, syncDir := writeLegacyTree(t)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := listener.Addr().String()
		_ = listener.Close()

		config := fmt.Sprintf(`[storage]
cold_storage_path = %q

[server]
listen_addr = %q

[[folders]]
label = "Photos"
syncthing_id = "isolation"
path = %q
budget = "1MB"
`, coldDir, addr, syncDir)
		configPath := filepath.Join(t.TempDir(), "coldstore.toml")
		if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command(coldstoreBinary(t), "--config", configPath)
		output := &syncedBuffer{}
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting the daemon: %v", err)
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		defer func() { _ = cmd.Process.Kill() }()

		deadline := time.Now().Add(30 * time.Second)
		up := false
		for time.Now().Before(deadline) && !up {
			resp, err := http.Get("http://" + addr + "/api/v1/status")
			if err == nil {
				up = resp.StatusCode == http.StatusOK
				_ = resp.Body.Close()
			}
			if !up {
				time.Sleep(50 * time.Millisecond)
			}
		}
		if !up {
			t.Fatalf("the daemon never answered /api/v1/status\n%s", output.String())
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
			t.Fatalf("the daemon did not stop on SIGTERM\n%s", output.String())
		}

		entries, err := os.ReadDir(coldDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			switch entry.Name() {
			case "journal", "blobs", "tmp", "views", "index.db", "index.db-wal", "index.db-shm", "control.sock":
				t.Errorf("the daemon created %s in cold storage although no collection is declared", entry.Name())
			}
		}
	})
}
