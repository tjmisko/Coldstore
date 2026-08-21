//go:build e2e_live

package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Every test in this file talks to the coldstore daemon running inside the
// container, over the published port, as a network client. Nothing constructs
// an api.Server, and nothing opens the catalog.

// writeManaged puts a file into the folder coldstore manages, from inside the
// coldstore container, and does not return until BOTH Syncthing devices have
// converged on it.
//
// It goes in through the filesystem rather than through the API because that is
// how files really arrive: Syncthing writes them.
//
// The wait is not politeness, it is the fix for a real 1-in-8 failure. That
// folder is device A's replicated tree, so Syncthing starts pushing the fixture
// to device B the moment it lands. A test that archives immediately is racing
// that push: if B is still settling when A's deletion is announced, B's version
// of the file is concurrent with the deletion rather than superseded by it, B
// sends the file back, and the managed folder the test just asserted was empty
// fills up again. Observed on cp-archive.bin -- "archived file's path is
// occupied again", once per scan, for the remainder of the run, poisoning every
// later test as well as the one that caused it.
//
// Waiting costs a few seconds per fixture and makes A's later deletion a strict
// successor of the only version B ever had.
func writeManaged(t *testing.T, rel string, content []byte) {
	t.Helper()
	live.writeFileIn(t, serviceColdstore, coldstoreSyncPath+"/"+rel, content)
	awaitFixtureConverged(t, rel)
}

// fixtureConvergeTimeout is generous for the same reason propagationTimeout is:
// index exchange, scanning and pulling are all asynchronous, and in practice a
// small fixture settles in 2-5 s.
const fixtureConvergeTimeout = 120 * time.Second

// awaitFixtureConverged blocks until device B holds rel on disk and neither
// device has any scanning, pulling or index work left on the shared folder.
func awaitFixtureConverged(t *testing.T, rel string) {
	t.Helper()
	await(t, fixtureConvergeTimeout,
		"both Syncthing devices to converge on the fixture "+rel+
			" before the test mutates it (see writeManaged)",
		func() bool {
			return live.existsIn(serviceSyncthingB, syncthingBPath(rel)) &&
				live.syncthingA.settled(liveSyncthingFolderID) &&
				live.syncthingB.settled(liveSyncthingFolderID)
		})
}

// ageManaged back-dates a file so it is outside any grace window. The grace
// check uses max(mtime, atime), so both have to move.
func ageManaged(t *testing.T, rel string) {
	t.Helper()
	live.exec(t, serviceColdstore, "touch", "-a", "-m", "-d", "2020-01-01T00:00:00",
		coldstoreSyncPath+"/"+rel)
}

func managedPath(rel string) string { return coldstoreSyncPath + "/" + rel }
func archivedPath(rel string) string {
	return coldstoreColdPath + "/archive/" + liveFolderLabel + "/" + rel
}

func TestLiveShouldRejectTheRequestWhenNoCredentialsAreSent(t *testing.T) {
	resp := live.requestNoAuth(t, http.MethodGet, "/api/v1/status")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/status without credentials: status %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Basic") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", got)
	}
}

func TestLiveShouldRejectTheRequestWhenThePasswordIsWrong(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, live.coldstore+"/api/v1/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(liveUsername, livePassword+"x")
	resp, err := live.http.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/status: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
}

func TestLiveShouldDescribeTheManagedFolderWhenStatusIsRequested(t *testing.T) {
	folder := live.folder(t, liveFolderLabel)

	if !folder.PresentInTOML {
		t.Error("present_in_toml = false; the folder is declared in docker/coldstore.test.toml")
	}
	if !folder.Enabled {
		t.Error("enabled = false; a freshly reconciled folder is enabled")
	}
	// 10GB in docker/coldstore.test.toml, in SI units.
	if want := int64(10_000_000_000); folder.BudgetBytes != want {
		t.Errorf("budget_bytes = %d, want %d (the value in docker/coldstore.test.toml)",
			folder.BudgetBytes, want)
	}
	if folder.GraceMinutes != 60 {
		t.Errorf("grace_minutes = %d, want 60", folder.GraceMinutes)
	}

	// The daemon is running against a real pool, so free space must be a real
	// number. Zero is what a statfs failure rendered as before it was fixed.
	status := live.status(t)
	if status.ColdStorageFreeBytes == 0 {
		t.Error("cold_storage_free_bytes = 0; the cold storage volume is not being measured")
	}
}

func TestLiveShouldCatalogueTheFileWhenAScanIsRequestedThroughTheAPI(t *testing.T) {
	const rel = "cp-scan.bin"
	content := randomBytes(48_000, 7)
	writeManaged(t, rel, content)

	live.scanAndWait(t)

	file, ok := live.fileByRelPath(t, rel)
	if !ok {
		t.Fatalf("no catalog record for %q after a completed scan job", rel)
	}
	if file.Status != "synced" {
		t.Errorf("status = %q, want synced", file.Status)
	}
	if file.SizeBytes != int64(len(content)) {
		t.Errorf("size_bytes = %d, want %d", file.SizeBytes, len(content))
	}
	if file.FolderLabel != liveFolderLabel {
		t.Errorf("folder_label = %q, want %q", file.FolderLabel, liveFolderLabel)
	}
}

func TestLiveShouldSkipSyncthingsOwnDirectoriesWhenScanning(t *testing.T) {
	// .stfolder is created by the real Syncthing on device A and contains a
	// marker file. If the scanner ever catalogued it, coldstore could archive
	// the marker and convince itself the volume had been unmounted.
	live.scanAndWait(t)

	var files []liveFile
	live.mustJSON(t, http.MethodGet,
		"/api/v1/files?folder="+liveFolderLabel+"&limit=500", nil, &files, http.StatusOK)

	for _, f := range files {
		if strings.HasPrefix(f.RelativePath, ".stfolder") ||
			strings.HasPrefix(f.RelativePath, ".stignore") ||
			strings.HasPrefix(f.RelativePath, ".stversions") {
			t.Errorf("catalogued Syncthing's own file %q", f.RelativePath)
		}
	}
}

func TestLiveShouldPersistTheNewPolicyWhenTheFolderIsEditedThroughTheAPI(t *testing.T) {
	before := live.folder(t, liveFolderLabel)
	t.Cleanup(func() {
		restore := map[string]any{
			"budget_bytes":  before.BudgetBytes,
			"grace_minutes": before.GraceMinutes,
			"enabled":       before.Enabled,
		}
		live.mustJSON(t, http.MethodPut, "/api/v1/folders/"+liveFolderLabel, restore, nil, http.StatusOK)
	})

	update := map[string]any{
		"budget_bytes":  int64(4_242_000),
		"grace_minutes": 7,
		"enabled":       true,
	}
	var updated liveFolderConfig
	live.mustJSON(t, http.MethodPut, "/api/v1/folders/"+liveFolderLabel, update, &updated, http.StatusOK)

	if updated.BudgetBytes != 4_242_000 {
		t.Errorf("PUT returned budget_bytes = %d, want 4242000", updated.BudgetBytes)
	}
	if updated.GraceMinutes != 7 {
		t.Errorf("PUT returned grace_minutes = %d, want 7", updated.GraceMinutes)
	}

	// The claim is that the DATABASE owns policy (D1), so the value has to come
	// back from a different endpoint on a different read path.
	after := live.folder(t, liveFolderLabel)
	if after.BudgetBytes != 4_242_000 || after.GraceMinutes != 7 {
		t.Errorf("GET /status shows budget=%d grace=%d, want 4242000 and 7",
			after.BudgetBytes, after.GraceMinutes)
	}

	var configs []liveFolderConfig
	live.mustJSON(t, http.MethodGet, "/api/v1/folders", nil, &configs, http.StatusOK)
	found := false
	for _, c := range configs {
		if c.FolderLabel != liveFolderLabel {
			continue
		}
		found = true
		if c.SyncthingID != liveSyncthingFolderID {
			t.Errorf("syncthing_id = %q, want %q (from the TOML)", c.SyncthingID, liveSyncthingFolderID)
		}
		if c.SyncthingPath != coldstoreSyncPath {
			t.Errorf("syncthing_path = %q, want %q", c.SyncthingPath, coldstoreSyncPath)
		}
	}
	if !found {
		t.Errorf("GET /api/v1/folders does not list %q", liveFolderLabel)
	}
}

func TestLiveShouldAnswerNotFoundWhenAnUnknownFolderIsEdited(t *testing.T) {
	resp := live.request(t, http.MethodPut, "/api/v1/folders/NoSuchFolder", map[string]any{
		"budget_bytes": int64(1000),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, want 404\nbody: %s", resp.StatusCode, body)
	}
}

func TestLiveShouldCountPinnedBytesAgainstTheBudgetWhenAFileIsPinned(t *testing.T) {
	const rel = "cp-pin.bin"
	content := randomBytes(64_000, 11)
	writeManaged(t, rel, content)
	live.scanAndWait(t)

	file, ok := live.fileByRelPath(t, rel)
	if !ok {
		t.Fatalf("no catalog record for %q", rel)
	}
	pinnedBefore := live.folder(t, liveFolderLabel).PinnedBytes

	var pinned struct {
		File    liveFile   `json:"file"`
		Folder  liveFolder `json:"folder"`
		Warning *string    `json:"warning"`
	}
	live.mustJSON(t, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/pin", file.ID), nil, &pinned, http.StatusOK)

	if !pinned.File.Pinned {
		t.Error("pinned = false after POST /pin")
	}
	if pinned.File.PinSource != "user" {
		t.Errorf("pin_source = %q, want user", pinned.File.PinSource)
	}
	// D3: pinned bytes are a subset of the folder's footprint and count against
	// the budget, so the folder block has to move by exactly this file's size.
	if got, want := pinned.Folder.PinnedBytes, pinnedBefore+int64(len(content)); got != want {
		t.Errorf("folder pinned_bytes = %d, want %d", got, want)
	}
	if pinned.Folder.PinnedBytes > pinned.Folder.SyncedSizeBytes {
		t.Errorf("pinned_bytes %d exceeds synced_size_bytes %d; pinned must be a subset",
			pinned.Folder.PinnedBytes, pinned.Folder.SyncedSizeBytes)
	}

	// And the pin is durable across a completely separate request.
	reread, _ := live.fileByRelPath(t, rel)
	if !reread.Pinned {
		t.Error("GET /files reports the file as unpinned right after it was pinned")
	}

	var unpinned struct {
		File   liveFile   `json:"file"`
		Folder liveFolder `json:"folder"`
	}
	live.mustJSON(t, http.MethodDelete, fmt.Sprintf("/api/v1/files/%d/pin", file.ID), nil, &unpinned, http.StatusOK)
	if unpinned.File.Pinned {
		t.Error("pinned = true after DELETE /pin")
	}
	if got := unpinned.Folder.PinnedBytes; got != pinnedBefore {
		t.Errorf("folder pinned_bytes = %d after unpin, want %d", got, pinnedBefore)
	}
}

func TestLiveShouldMoveTheBytesToColdStorageWhenAFileIsArchivedThroughTheAPI(t *testing.T) {
	const rel = "cp-archive.bin"
	content := randomBytes(96_000, 19)
	want := sha256Hex(content)
	writeManaged(t, rel, content)
	live.scanAndWait(t)

	file, ok := live.fileByRelPath(t, rel)
	if !ok {
		t.Fatalf("no catalog record for %q", rel)
	}

	var archived liveFile
	live.mustJSON(t, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/archive", file.ID), nil, &archived, http.StatusOK)
	if archived.Status != "archived" {
		t.Fatalf("status = %q after archive, want archived", archived.Status)
	}

	// On disk, in the container, not in the catalog's opinion.
	if live.existsIn(serviceColdstore, managedPath(rel)) {
		t.Errorf("%s still exists in the managed folder after archiving", managedPath(rel))
	}
	if got := live.sha256In(serviceColdstore, archivedPath(rel)); got != want {
		t.Errorf("archived copy sha256 = %q, want %q (the bytes did not survive the move)", got, want)
	}
	// The temp sibling the cross-device copy writes through must not survive.
	for _, name := range live.listDirIn(t, serviceColdstore, coldstoreColdPath+"/archive/"+liveFolderLabel) {
		if strings.HasSuffix(name, tmpSuffixLive) {
			t.Errorf("cold storage still holds the temp sibling %q", name)
		}
	}

	// An archived file is still downloadable -- that is what the catalog is for.
	resp := live.request(t, http.MethodGet, fmt.Sprintf("/api/v1/files/%d/download", file.ID), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download after archive: status %d, want 200", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("downloaded %d bytes, want the original %d and identical", len(got), len(content))
	}
}

func TestLiveShouldPutTheBytesBackAndPinThemWhenAFileIsRestoredThroughTheAPI(t *testing.T) {
	const rel = "cp-restore.bin"
	content := randomBytes(72_000, 23)
	want := sha256Hex(content)
	writeManaged(t, rel, content)
	live.scanAndWait(t)

	file, ok := live.fileByRelPath(t, rel)
	if !ok {
		t.Fatalf("no catalog record for %q", rel)
	}
	mtimeBefore := live.exec(t, serviceColdstore, "stat", "-c", "%Y", managedPath(rel))

	var archived liveFile
	live.mustJSON(t, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/archive", file.ID), nil, &archived, http.StatusOK)

	var restored liveFile
	live.mustJSON(t, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/restore", file.ID), nil, &restored, http.StatusOK)

	if restored.Status != "synced" {
		t.Errorf("status = %q after restore, want synced", restored.Status)
	}
	// D2: a restore pins, so the next quota tick cannot immediately undo the
	// thing the owner just asked for.
	if !restored.Pinned {
		t.Error("pinned = false after a restore; D2 requires a restore to pin")
	}
	if restored.PinSource != "restore" {
		t.Errorf("pin_source = %q after a restore, want restore", restored.PinSource)
	}

	if got := live.sha256In(serviceColdstore, managedPath(rel)); got != want {
		t.Errorf("restored file sha256 = %q, want %q", got, want)
	}
	if live.existsIn(serviceColdstore, archivedPath(rel)) {
		t.Errorf("%s still exists in cold storage after a restore", archivedPath(rel))
	}
	// mtime is preserved through both moves. Without that, a restore re-dates
	// the photo to today and Syncthing propagates the new date everywhere.
	mtimeAfter := live.exec(t, serviceColdstore, "stat", "-c", "%Y", managedPath(rel))
	if strings.TrimSpace(mtimeBefore) != strings.TrimSpace(mtimeAfter) {
		t.Errorf("mtime changed across archive+restore: %s -> %s",
			strings.TrimSpace(mtimeBefore), strings.TrimSpace(mtimeAfter))
	}
	for _, name := range live.listDirIn(t, serviceColdstore, coldstoreSyncPath) {
		if strings.HasSuffix(name, tmpSuffixLive) {
			t.Errorf("the managed folder still holds the temp sibling %q after a restore", name)
		}
	}
}

// Invariant 12: archive and restore are idempotent. Asking for the state a file
// is already in is a no-op success, not a conflict. That is what makes a retried
// job item -- after a crash, after a resumed queue, after an impatient double
// click -- safe.
func TestLiveShouldSucceedWithoutMovingAnythingWhenTheFileIsAlreadyInTheRequestedState(t *testing.T) {
	const rel = "cp-idempotent.bin"
	content := randomBytes(8_000, 29)
	want := sha256Hex(content)
	writeManaged(t, rel, content)
	live.scanAndWait(t)

	file, ok := live.fileByRelPath(t, rel)
	if !ok {
		t.Fatalf("no catalog record for %q", rel)
	}
	restorePath := fmt.Sprintf("/api/v1/files/%d/restore", file.ID)
	archivePath := fmt.Sprintf("/api/v1/files/%d/archive", file.ID)

	var got liveFile
	live.mustJSON(t, http.MethodPost, restorePath, nil, &got, http.StatusOK)
	if got.Status != "synced" {
		t.Errorf("restoring an already-synced file left status %q, want synced", got.Status)
	}
	if got.Pinned {
		t.Error("restoring an already-synced file pinned it; the no-op path must not touch the row")
	}
	if h := live.sha256In(serviceColdstore, managedPath(rel)); h != want {
		t.Errorf("the file changed on disk during a no-op restore: sha256 %q, want %q", h, want)
	}

	live.mustJSON(t, http.MethodPost, archivePath, nil, &got, http.StatusOK)
	live.mustJSON(t, http.MethodPost, archivePath, nil, &got, http.StatusOK)
	if got.Status != "archived" {
		t.Errorf("archiving twice left status %q, want archived", got.Status)
	}
	if h := live.sha256In(serviceColdstore, archivedPath(rel)); h != want {
		t.Errorf("the archived copy changed during a repeated archive: sha256 %q, want %q", h, want)
	}

	live.mustJSON(t, http.MethodPost, restorePath, nil, &got, http.StatusOK)
	if got.Status != "synced" {
		t.Errorf("status %q after restoring, want synced", got.Status)
	}
}

func TestLiveShouldAnswerNotFoundWhenTheFileIdDoesNotExist(t *testing.T) {
	for _, path := range []string{
		"/api/v1/files/999999/restore",
		"/api/v1/files/999999/archive",
		"/api/v1/files/999999/pin",
	} {
		resp := live.request(t, http.MethodPost, path, nil)
		if resp.StatusCode != http.StatusNotFound {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("POST %s: status %d, want 404\nbody: %s", path, resp.StatusCode, body)
		}
		resp.Body.Close()
	}
}

func TestLiveShouldArchiveEveryFileInTheSelectionWhenABulkJobIsSubmitted(t *testing.T) {
	const count = 5
	rels := make([]string, count)
	ids := make([]int64, 0, count)
	for i := range rels {
		rels[i] = fmt.Sprintf("cp-bulk-%d.bin", i)
		writeManaged(t, rels[i], randomBytes(20_000+i*1_000, byte(31+i)))
	}
	live.scanAndWait(t)

	for _, rel := range rels {
		file, ok := live.fileByRelPath(t, rel)
		if !ok {
			t.Fatalf("no catalog record for %q", rel)
		}
		ids = append(ids, file.ID)
	}

	var created liveJobCreated
	live.mustJSON(t, http.MethodPost, "/api/v1/jobs",
		map[string]any{"action": "archive", "ids": ids}, &created, http.StatusAccepted)
	if created.Total != count {
		t.Errorf("job total = %d, want %d", created.Total, count)
	}

	job := live.waitForJob(t, created.JobID, 120*time.Second)
	if job.State != "done" {
		t.Fatalf("archive job finished %q with %d failures: %+v", job.State, job.FailedCount, job.Failed)
	}
	if job.Done != count {
		t.Errorf("job done = %d, want %d", job.Done, count)
	}

	for _, rel := range rels {
		if live.existsIn(serviceColdstore, managedPath(rel)) {
			t.Errorf("%s is still on device after a completed bulk archive", rel)
		}
		if !live.existsIn(serviceColdstore, archivedPath(rel)) {
			t.Errorf("%s is not in cold storage after a completed bulk archive", rel)
		}
	}

	// And back again. force=true skips the budget pre-flight; the budget here
	// is 10GB, so the pre-flight would pass anyway -- the flag keeps this test
	// about the queue rather than about section 5.10.
	var restoreJob liveJobCreated
	live.mustJSON(t, http.MethodPost, "/api/v1/jobs",
		map[string]any{"action": "restore", "ids": ids, "force": true}, &restoreJob, http.StatusAccepted)

	done := live.waitForJob(t, restoreJob.JobID, 120*time.Second)
	if done.State != "done" || done.FailedCount != 0 {
		t.Fatalf("restore job finished %q with %d failures: %+v", done.State, done.FailedCount, done.Failed)
	}
	for _, rel := range rels {
		if !live.existsIn(serviceColdstore, managedPath(rel)) {
			t.Errorf("%s did not come back after a completed bulk restore", rel)
		}
	}

	// The job survives as a record the UI can list.
	var jobs []liveJob
	live.mustJSON(t, http.MethodGet, "/api/v1/jobs?limit=50", nil, &jobs, http.StatusOK)
	seen := false
	for _, j := range jobs {
		if j.ID == created.JobID {
			seen = true
		}
	}
	if !seen {
		t.Errorf("job %d is missing from GET /api/v1/jobs", created.JobID)
	}
}

func TestLiveShouldRejectTheJobWhenTheSelectionIsEmpty(t *testing.T) {
	resp := live.request(t, http.MethodPost, "/api/v1/jobs",
		map[string]any{"action": "archive", "ids": []int64{}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

func TestLiveShouldGenerateAndServeAThumbnailWhenTheFileIsAVideo(t *testing.T) {
	// The one test that proves ffmpeg is actually present and working in the
	// shipped image, which no in-process test can tell you.
	const rel = "cp-thumb.mp4"
	live.exec(t, serviceColdstore, "cp", "/app/fixtures/minimal.mp4", managedPath(rel))
	live.scanAndWait(t)

	// The thumbnail pass runs off the catalog-sync ticker (1 minute in
	// docker/coldstore.test.toml), not off the scan job, so this waits for a
	// tick rather than for a request.
	file := live.awaitFile(t, rel, 3*time.Minute, "a generated thumbnail", func(f liveFile) bool {
		return f.ThumbnailPath != ""
	})

	resp := live.request(t, http.MethodGet, "/api/v1/thumbnails/"+file.ThumbnailPath, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET thumbnail %q: status %d, want 200", file.ThumbnailPath, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 100 || body[0] != 0xFF || body[1] != 0xD8 {
		t.Errorf("thumbnail is %d bytes starting %x; want a JPEG (FFD8...)", len(body), body[:min(4, len(body))])
	}
}
