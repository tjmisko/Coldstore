// Package archivetier is the integration tier of the archive core: the crash
// matrix, rebuild equality, corruption end to end and legacy isolation. It has
// no build tag, so `go test ./...` runs it. Every test uses small fixtures
// (kilobytes) and no ffmpeg or poppler.
package archivetier

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/api"
	"github.com/tjmisko/coldstore/internal/archive"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/crashpoint"
	"github.com/tjmisko/coldstore/internal/fsx"
	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/store"
	"github.com/tjmisko/coldstore/internal/views"
)

const (
	collectionName = "Scans"

	envRoot     = "COLDSTORE_TIER_ROOT"
	envSource   = "COLDSTORE_TIER_SOURCE"
	envScenario = "COLDSTORE_SCENARIO"

	scenarioImport = "import"
	scenarioUpload = "upload"

	listenPrefix = "LISTEN "
)

// ---------------------------------------------------------------------------
// the child process

// TestCrashChild is not a test. The parent re-executes the test binary with
// -test.run=^TestCrashChild$ and COLDSTORE_CRASHPOINT set; the child opens the
// archive, runs the named scenario and is killed by SIGKILL at the armed point.
// Run normally it is skipped.
func TestCrashChild(t *testing.T) {
	scenario := os.Getenv(envScenario)
	if scenario == "" {
		t.Skip("only runs as a re-executed child of the crash matrix")
	}
	armed, err := crashpoint.ArmFromEnv()
	if err != nil {
		t.Fatalf("arming the crash point: %v", err)
	}
	if !armed {
		t.Fatal("the child was started without a crash point")
	}
	root := os.Getenv(envRoot)
	service, err := archive.Open(context.Background(), serviceOptions(root))
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	switch scenario {
	case scenarioImport:
		childImport(t, service)
	case scenarioUpload:
		childUpload(t, service, root)
	default:
		t.Fatalf("unknown scenario %q", scenario)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func childImport(t *testing.T, service *archive.Service) {
	id, err := service.ImportStart(collectionName, os.Getenv(envSource))
	if err != nil {
		t.Fatalf("ImportStart: %v", err)
	}
	for {
		status, ok := service.ImportStatus(id)
		if !ok {
			t.Fatalf("import %d vanished", id)
		}
		if status.State != "running" {
			if status.FailedCount != 0 {
				t.Fatalf("import failed %d files: %+v", status.FailedCount, status.Failed)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func childUpload(t *testing.T, service *archive.Service, root string) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := api.NewServer(api.Deps{Cfg: apiConfig(root), Logger: quietLogger(), Archive: service})
	go func() { _ = http.Serve(listener, server.Handler()) }()
	fmt.Fprintf(os.Stdout, "%s%s\n", listenPrefix, listener.Addr().String())
	// The parent closes stdin when it has sent everything: a clean exit means
	// the armed point was never reached.
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = listener.Close()
}

// childRun is the outcome of one child.
type childRun struct {
	Died   bool // killed by SIGKILL
	Stderr string
	Exit   error
}

// startChild launches the re-executed test binary. The caller waits on it.
func startChild(t *testing.T, scenario, point string, hit int, root, source string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		envScenario+"="+scenario,
		envRoot+"="+root,
		envSource+"="+source,
		crashpoint.EnvPoint+"="+point,
		fmt.Sprintf("%s=%d", crashpoint.EnvHit, hit),
	)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	return cmd, stderr
}

func finishChild(t *testing.T, cmd *exec.Cmd, stderr *bytes.Buffer) childRun {
	t.Helper()
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		return classifyExit(err, stderr)
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		<-waitErr
		t.Fatalf("the child did not exit within 60s\n%s", stderr.String())
		return childRun{}
	}
}

func classifyExit(err error, stderr *bytes.Buffer) childRun {
	run := childRun{Stderr: stderr.String(), Exit: err}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return run
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	run.Died = ok && status.Signaled() && status.Signal() == syscall.SIGKILL
	return run
}

// runImportChild runs the import scenario to completion or to its death.
func runImportChild(t *testing.T, point string, hit int, root, source string) childRun {
	t.Helper()
	cmd, stderr := startChild(t, scenarioImport, point, hit, root, source)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	return finishChild(t, cmd, stderr)
}

// runUploadChild serves the API in a child, sends every file in order and
// reports whether the child died. Uploads stop at the first connection error.
func runUploadChild(t *testing.T, point string, hit int, root string, files []sourceFile) childRun {
	t.Helper()
	cmd, stderr := startChild(t, scenarioUpload, point, hit, root, "")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	addr := readListenAddress(t, cmd, stdout, stderr)
	client := &http.Client{Timeout: 20 * time.Second}
	for _, file := range files {
		if _, _, err := putFile(client, "http://"+addr, collectionName, file); err != nil {
			break // the daemon died under this request
		}
	}
	_ = stdin.Close()
	return finishChild(t, cmd, stderr)
}

func readListenAddress(t *testing.T, cmd *exec.Cmd, stdout io.Reader, stderr *bytes.Buffer) string {
	t.Helper()
	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if addr, ok := strings.CutPrefix(scanner.Text(), listenPrefix); ok {
				found <- addr
				break
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	select {
	case addr := <-found:
		return addr
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("the upload child never printed its listen address\n%s", stderr.String())
		return ""
	}
}

// ---------------------------------------------------------------------------
// the source tree

type sourceFile struct {
	Path string
	Body string
}

var sourceMtime = time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)

// demoSource is nine distinct contents at ten paths: Copies/again.txt repeats
// the bytes of Letters/a.txt, which the resolve rules answer with a Copy (the
// same Item, a second Placement).
func demoSource() []sourceFile {
	files := []sourceFile{
		{"Letters/a.txt", "alpha: the first letter\n"},
		{"Letters/b.txt", "bravo: the second letter, a little longer than the first\n"},
		{"Letters/c.txt", "charlie\n"},
		{"Trips/2024 Alps/IMG 001.jpg", strings.Repeat("jpeg-ish bytes 001 ", 40)},
		{"Trips/2024 Alps/IMG 002.jpg", strings.Repeat("jpeg-ish bytes 002 ", 40)},
		{"Trips/2025 Coast/notes.md", "# Coast\nsalt and gulls\n"},
		{"Receipts/2025-01.pdf", "%PDF-1.4 not really a pdf, receipt one\n"},
		{"Receipts/2025-02.pdf", "%PDF-1.4 not really a pdf, receipt two\n"},
		{"top-level.bin", string([]byte{0, 1, 2, 3, 250, 251, 252, 253, 254, 255})},
		{"Copies/again.txt", "alpha: the first letter\n"},
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func distinctContents(files []sourceFile) int {
	seen := map[string]bool{}
	for _, file := range files {
		seen[file.Body] = true
	}
	return len(seen)
}

func writeSource(t *testing.T, files []sourceFile) string {
	t.Helper()
	dir := t.TempDir()
	for _, file := range files {
		full := filepath.Join(dir, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(file.Body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, sourceMtime, sourceMtime); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// treeSnapshot is path, size, mtime and sha256 of every file under dir, sorted.
func treeSnapshot(t *testing.T, dir string) []string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		rel, _ := filepath.Rel(dir, path)
		lines = append(lines, fmt.Sprintf("%s|%d|%s|%s", rel, info.Size(),
			info.ModTime().UTC().Format(time.RFC3339Nano), hex.EncodeToString(sum[:])))
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotting %s: %v", dir, err)
	}
	sort.Strings(lines)
	return lines
}

// sourceTree is the directory an import reads and what it looked like before
// anything touched the archive.
type sourceTree struct {
	Dir    string
	Files  []sourceFile
	Before []string
}

func newSourceTree(t *testing.T, files []sourceFile) *sourceTree {
	t.Helper()
	dir := writeSource(t, files)
	return &sourceTree{Dir: dir, Files: files, Before: treeSnapshot(t, dir)}
}

// ---------------------------------------------------------------------------
// opening the archive

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func serviceOptions(root string) archive.Options {
	return archive.Options{
		Root:        root,
		Collections: []string{collectionName},
		Logger:      quietLogger(),
		Sleep:       func(time.Duration) {},
	}
}

func openService(t *testing.T, opts archive.Options) *archive.Service {
	t.Helper()
	service, err := archive.Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func closeService(t *testing.T, service *archive.Service) {
	t.Helper()
	if err := service.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// newRoot makes an archive root with the collection declared, so a child's
// first hit of a point is in the scenario and not in Open.
func newRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "archive")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	closeService(t, openService(t, serviceOptions(root)))
	return root
}

func apiConfig(root string) *config.Config {
	return &config.Config{
		Storage:     config.StorageConfig{ColdStoragePath: root},
		Server:      config.ServerConfig{ListenAddr: "127.0.0.1:0"},
		Collections: []config.CollectionConfig{{Name: collectionName}},
	}
}

func newAPIServer(t *testing.T, root string, service *archive.Service) *httptest.Server {
	t.Helper()
	server := api.NewServer(api.Deps{Cfg: apiConfig(root), Logger: quietLogger(), Archive: service})
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// ---------------------------------------------------------------------------
// HTTP helpers

func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(part, "%", "%25"), " ", "%20")
	}
	return strings.Join(parts, "/")
}

func hashHex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// putFile uploads one file and returns the status and body.
func putFile(client *http.Client, baseURL, collection string, file sourceFile) (int, string, error) {
	req, err := http.NewRequest(http.MethodPut,
		baseURL+"/api/v1/collections/"+collection+"/files/"+escapePath(file.Path), strings.NewReader(file.Body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("X-Coldstore-Sha256", hashHex(file.Body))
	req.Header.Set("X-Coldstore-Mtime", sourceMtime.Format(time.RFC3339))
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), err
}

func mustPutFile(t *testing.T, baseURL string, file sourceFile) {
	t.Helper()
	status, body, err := putFile(&http.Client{Timeout: 20 * time.Second}, baseURL, collectionName, file)
	if err != nil {
		t.Fatalf("PUT %s: %v", file.Path, err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("PUT %s = %d %s", file.Path, status, body)
	}
}

// ---------------------------------------------------------------------------
// reading the archive

func parseHash(t *testing.T, hexHash string) store.Hash {
	t.Helper()
	hash, err := store.ParseHash(hexHash)
	if err != nil {
		t.Fatalf("ParseHash(%q): %v", hexHash, err)
	}
	return hash
}

func allEntries(t *testing.T, ix *index.Index, trashed *bool) []index.Entry {
	t.Helper()
	entries, err := ix.Entries(index.EntryQuery{Folder: collectionName, Trashed: trashed, Limit: index.MaxPageLimit})
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	return entries
}

// countItems is the number of distinct Items behind the collection's
// placements, live and trashed.
func countItems(t *testing.T, ix *index.Index) int {
	t.Helper()
	yes := true
	items := map[journal.ItemID]bool{}
	for _, entry := range allEntries(t, ix, nil) {
		items[entry.Item] = true
	}
	for _, entry := range allEntries(t, ix, &yes) {
		items[entry.Item] = true
	}
	return len(items)
}

func dumpIndex(t *testing.T, ix *index.Index) string {
	t.Helper()
	var buf bytes.Buffer
	if err := ix.Dump(&buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	return buf.String()
}

func viewsTree(root string, service *archive.Service) *views.Tree {
	return views.New(fsx.OS(), filepath.Join(root, "views"), service.Store().Path)
}

func manifestOf(t *testing.T, tree *views.Tree, ix *index.Index) string {
	t.Helper()
	var buf bytes.Buffer
	if err := tree.Manifest(context.Background(), ix, &buf); err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	return buf.String()
}

func regenerateViews(t *testing.T, tree *views.Tree, ix *index.Index) {
	t.Helper()
	if _, err := tree.Regenerate(context.Background(), ix); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
}

// rebuildDump replays the journal into a scratch index and dumps it. The caller
// must not hold the journal lock.
func rebuildDump(t *testing.T, root string) string {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(root, "journal")
	lock, err := journal.AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer func() { _ = lock.Release() }()
	log, err := journal.Open(ctx, fsx.OS(), dir, journal.Options{Logger: quietLogger()})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer func() { _ = log.Close() }()
	rebuilt, err := index.Rebuild(ctx, log, filepath.Join(t.TempDir(), "rebuilt.db"))
	if err != nil {
		t.Fatalf("index.Rebuild: %v", err)
	}
	defer func() { _ = rebuilt.Close() }()
	return dumpIndex(t, rebuilt)
}

// journalBlobHashes replays the journal and returns every hash a blob_added
// record names.
func journalBlobHashes(t *testing.T, root string) []string {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(root, "journal")
	lock, err := journal.AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer func() { _ = lock.Release() }()
	log, err := journal.Open(ctx, fsx.OS(), dir, journal.Options{Logger: quietLogger()})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer func() { _ = log.Close() }()
	var hashes []string
	err = log.Replay(ctx, 0, func(rec journal.Record) error {
		if rec.Type == journal.TypeBlobAdded {
			hashes = append(hashes, rec.Hash)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return hashes
}

// appendRecords appends to the journal of a root nobody has open.
func appendRecords(t *testing.T, root string, recs ...journal.Record) {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(root, "journal")
	lock, err := journal.AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer func() { _ = lock.Release() }()
	log, err := journal.Open(ctx, fsx.OS(), dir, journal.Options{Logger: quietLogger()})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer func() { _ = log.Close() }()
	if _, err := log.Append(ctx, recs...); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

// ---------------------------------------------------------------------------
// the invariant checker

// checkInvariants asserts, after a crash, that the root is sound. source may be
// nil when the scenario has no source tree.
//
//  1. archive.Open succeeds (replay heals the index).
//  2. Every blob_added in the journal verifies: no record names a missing blob.
//  3. The source tree equals its pre-run snapshot.
//  4. The index dump equals the dump of a fresh Rebuild of the same journal.
//  5. views.Manifest equals the manifest after views.Regenerate.
func checkInvariants(t *testing.T, root string, source *sourceTree) {
	t.Helper()
	service, err := archive.Open(context.Background(), serviceOptions(root))
	if err != nil {
		t.Fatalf("invariant 1: archive.Open after the crash: %v", err)
	}
	liveDump := dumpIndex(t, service.Index())
	tree := viewsTree(root, service)
	manifestBefore := manifestOf(t, tree, service.Index())
	regenerateViews(t, tree, service.Index())
	manifestAfter := manifestOf(t, tree, service.Index())
	closeService(t, service)

	if manifestBefore != manifestAfter {
		t.Errorf("invariant 5: views.Manifest differs from the manifest after Regenerate\nbefore:\n%s\nafter:\n%s",
			manifestBefore, manifestAfter)
	}

	st, err := store.Open(fsx.OS(), root)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	for _, hexHash := range journalBlobHashes(t, root) {
		if err := st.Verify(context.Background(), parseHash(t, hexHash)); err != nil {
			t.Errorf("invariant 2: journaled blob %s does not verify: %v", hexHash, err)
		}
	}

	if source != nil {
		after := treeSnapshot(t, source.Dir)
		if strings.Join(after, "\n") != strings.Join(source.Before, "\n") {
			t.Errorf("invariant 3: the source tree changed\nbefore: %v\nafter:  %v", source.Before, after)
		}
	}

	if rebuilt := rebuildDump(t, root); rebuilt != liveDump {
		t.Errorf("invariant 4: the index differs from a rebuild of the journal\nlive:\n%s\nrebuilt:\n%s",
			firstLines(liveDump, 40), firstLines(rebuilt, 40))
	}
}

func firstLines(text string, n int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	for i, line := range lines {
		if len(line) > 200 {
			lines[i] = line[:200] + "..."
		}
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// the crash matrix

var crashFamilies = []string{"store.put.", "journal.append.", "journal.rotate.", "index.apply.", "views.link.", "ingest."}

// requiredCrashPoints are the names the plan's contract C2 promises. A point
// that disappears must fail the matrix, not shrink it.
var requiredCrashPoints = []string{
	"store.put.staged_created", "store.put.staged_written", "store.put.staged_synced",
	"store.put.staged_verified", "store.put.linked", "store.put.dir_synced", "store.put.tmp_removed",
	"journal.append.before_write", "journal.append.written", "journal.append.synced",
	"journal.rotate.segment_created", "journal.rotate.dir_synced",
	"index.apply.before_commit", "index.apply.committed",
	"views.link.staged", "views.link.renamed",
	"ingest.arrival.opened", "ingest.put.done", "ingest.journal.appended",
	"ingest.index.applied", "ingest.views.synced",
}

func matrixPoints(t *testing.T) []string {
	t.Helper()
	registered := map[string]bool{}
	var points []string
	for _, name := range crashpoint.Names() {
		registered[name] = true
		for _, family := range crashFamilies {
			if strings.HasPrefix(name, family) {
				points = append(points, name)
				break
			}
		}
	}
	for _, name := range requiredCrashPoints {
		if !registered[name] {
			t.Errorf("the required crash point %q is not registered", name)
		}
	}
	return points
}

func isRotatePoint(point string) bool { return strings.HasPrefix(point, "journal.rotate.") }

// hitsFor is hit 1 and hit 3. A rotate point can only be hit once in a run: a
// segment is 16 MiB and the fixture fills exactly one, so the second rotation
// would need 16 MiB more.
func hitsFor(point string) []int {
	if isRotatePoint(point) {
		return []int{1}
	}
	return []int{1, 3}
}

const (
	preseedRecordBytes = 1_000_000
	preseedRecords     = 17 // 17 x ~1 MB passes the 16 MiB segment size
)

// preseedFullSegment fills the journal's only segment past 16 MiB, so the next
// Append rotates. It describes one real Item over and over: each record is a
// valid, replayable fact.
func preseedFullSegment(t *testing.T, root string) {
	t.Helper()
	service := openService(t, serviceOptions(root))
	seedFile := sourceFile{"seed/seed.txt", "the item the preseed describes\n"}
	ts := newAPIServer(t, root, service)
	mustPutFile(t, ts.URL, seedFile)
	entries := allEntries(t, service.Index(), nil)
	if len(entries) != 1 {
		t.Fatalf("expected one seed entry, got %d", len(entries))
	}
	item := entries[0].Item
	closeService(t, service)

	text := strings.Repeat("d", preseedRecordBytes)
	for i := 0; i < preseedRecords; i++ {
		appendRecords(t, root, journal.Record{By: "preseed", Type: journal.TypeItemDescribed, Item: item, Text: &text})
	}
}

// prepareRoot is a fresh root, with a near-full segment when the point needs one.
func prepareRoot(t *testing.T, point string) string {
	t.Helper()
	root := newRoot(t)
	if isRotatePoint(point) {
		preseedFullSegment(t, root)
	}
	return root
}

func baselineItems(t *testing.T, root string) int {
	t.Helper()
	service := openService(t, serviceOptions(root))
	count := countItems(t, service.Index())
	closeService(t, service)
	return count
}

// ---------------------------------------------------------------------------
// the coldstore binary

var (
	binaryOnce sync.Once
	binaryPath string
	binaryErr  error
)

// coldstoreBinary builds cmd/coldstore once per test run, with the default Go
// build cache.
func coldstoreBinary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "coldstore-tier-bin-")
		if err != nil {
			binaryErr = err
			return
		}
		binaryPath = filepath.Join(dir, "coldstore")
		build := exec.Command("go", "build", "-buildvcs=false", "-o", binaryPath, "github.com/tjmisko/coldstore/cmd/coldstore")
		if out, err := build.CombinedOutput(); err != nil {
			binaryErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if binaryErr != nil {
		t.Fatalf("building the coldstore binary: %v", binaryErr)
	}
	return binaryPath
}

// writeColdstoreConfig writes a minimal coldstore.toml next to root.
func writeColdstoreConfig(t *testing.T, root, extra string) string {
	t.Helper()
	text := fmt.Sprintf("[storage]\ncold_storage_path = %q\n\n[server]\nlisten_addr = \"127.0.0.1:0\"\n\n%s", root, extra)
	path := filepath.Join(t.TempDir(), "coldstore.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func collectionConfigBlock() string {
	return fmt.Sprintf("[[collections]]\nname = %q\n", collectionName)
}
