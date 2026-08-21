//go:build e2e_live

// Package e2e's LIVE tier. Everything in a file tagged e2e_live drives the
// coldstore daemon running inside docker/compose.test.yml over its published
// HTTP port, and drives two real Syncthing daemons over their REST APIs.
//
// It shares nothing with the e2e tier: no in-process api.Server, no
// httptest.Server, no catalog handle, no direct filesystem access to anything
// the daemon owns. Every assertion here is made through an interface a real
// client has -- HTTP for coldstore, HTTP for Syncthing, and `docker compose
// exec` for "is the byte actually on that device's disk".
//
// The tags are exclusive on purpose. `go test -tags e2e_live` builds ONLY these
// files. The old target ran `-tags e2e,e2e_live`, which rebuilt and re-ran the
// entire in-process suite and never once opened a socket to the container; the
// separation is what makes a green live run mean something.
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	// composeProject namespaces every container, network and volume this suite
	// creates, so a stray `docker compose down` in the repo cannot take out an
	// unrelated stack and vice versa.
	composeProject = "coldstore-live"

	serviceColdstore  = "coldstore-test"
	serviceSyncthingA = "syncthing-a"
	serviceSyncthingB = "syncthing-b"

	// liveFolderLabel is coldstore's label for the managed folder; it must match
	// docker/coldstore.test.toml.
	liveFolderLabel = "TestSync"

	// liveSyncthingFolderID is the Syncthing folder id the harness creates on
	// both devices; it must match `syncthing_id` in docker/coldstore.test.toml.
	liveSyncthingFolderID = "livesync"

	// devicePathA is where the managed folder is mounted inside each container.
	// Same volume, two mount points -- that is the whole arrangement under test.
	coldstoreSyncPath = "/data/sync"
	coldstoreColdPath = "/data/cold"
	syncthingSyncPath = "/var/syncthing/sync"

	liveUsername = "coldstore-test"
	livePassword = "coldstore-test"

	// tmpSuffixLive duplicates internal/daemon's unexported tmpSuffix on
	// purpose: this suite may not import daemon internals, and a live test that
	// silently followed a rename of that constant would stop checking anything.
	tmpSuffixLive = ".coldstore-tmp"
)

// Environment knobs, all with working defaults.
//
//	COLDSTORE_TEST_PORT       host port for the coldstore API      (18090)
//	SYNCTHING_A_PORT          host port for device A's REST API    (18384)
//	SYNCTHING_B_PORT          host port for device B's REST API    (18385)
//	SYNCTHING_API_KEY         X-API-Key for both devices           (coldstore-live-test-key)
//	COLDSTORE_LIVE_SKIP_BUILD skip `go build` of the binary the image copies
//	COLDSTORE_LIVE_SKIP_UP    assume the stack is already up and paired
//	COLDSTORE_LIVE_SKIP_PAIR  do not (re)pair the devices; leaves an injected fault in place
//	COLDSTORE_LIVE_KEEP       leave the stack running after the suite
//	COLDSTORE_LIVE_COMPOSE    alternate compose file (used by the fault-injection run)
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envTrue(key string) bool {
	v := strings.ToLower(os.Getenv(key))
	return v == "1" || v == "true" || v == "yes"
}

// stack is the process-wide handle to the running containers. TestMain fills it
// in before any test runs; nothing else writes to it.
type stack struct {
	repoRoot    string
	composeFile string
	coldstore   string // http://127.0.0.1:PORT
	syncthingA  *syncthingClient
	syncthingB  *syncthingClient
	http        *http.Client
}

var live stack

// ---------------------------------------------------------------------------
// docker compose
// ---------------------------------------------------------------------------

func (s *stack) composeArgs(args ...string) []string {
	return append([]string{"compose", "-p", composeProject, "-f", s.composeFile}, args...)
}

func (s *stack) composeCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", s.composeArgs(args...)...)
	cmd.Dir = s.repoRoot
	cmd.Env = os.Environ()
	return cmd
}

// composeRun runs a compose subcommand and returns combined output. Compose
// writes progress to stderr, so combined output is what a human wants when this
// fails.
func (s *stack) composeRun(ctx context.Context, args ...string) (string, error) {
	out, err := s.composeCmd(ctx, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w", strings.Join(s.composeArgs(args...), " "), err)
	}
	return string(out), nil
}

// exec runs a command inside a service container and returns its stdout.
//
// This is the suite's only window onto a device's actual disk, and it is a
// deliberate one: asserting "the file is gone from the phone" has to look at the
// phone, not at coldstore's opinion of the phone.
func (s *stack) exec(t *testing.T, service string, argv ...string) string {
	t.Helper()
	out, err := s.execErr(service, nil, argv...)
	if err != nil {
		t.Fatalf("exec in %s: %v\n%s", service, err, out)
	}
	return out
}

func (s *stack) execErr(service string, stdin io.Reader, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := s.composeCmd(ctx, append([]string{"exec", "-T", service}, argv...)...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String(), nil
}

// execOK reports whether a command inside a container succeeded, where a
// non-zero exit is an answer rather than an error.
func (s *stack) execOK(service string, argv ...string) bool {
	_, err := s.execErr(service, nil, argv...)
	return err == nil
}

// existsIn reports whether a path exists inside a container. It goes through
// `sh -c` rather than exec'ing `test` directly: the two images are Debian and
// Alpine, and only one of them has a standalone /usr/bin/test.
func (s *stack) existsIn(service, path string) bool {
	return s.execOK(service, "sh", "-c", "test -e '"+path+"'")
}

func (s *stack) isDirIn(service, path string) bool {
	return s.execOK(service, "sh", "-c", "test -d '"+path+"'")
}

// writeFileIn streams content into a file inside a container. Content comes from
// the test process so the test knows the exact bytes it is later going to
// demand back.
func (s *stack) writeFileIn(t *testing.T, service, path string, content []byte) {
	t.Helper()
	dir := filepath.Dir(path)
	if out, err := s.execErr(service, nil, "mkdir", "-p", dir); err != nil {
		t.Fatalf("mkdir %s in %s: %v\n%s", dir, service, err, out)
	}
	// `cat > path` rather than a shell heredoc: the content is arbitrary binary.
	out, err := s.execErr(service, bytes.NewReader(content), "sh", "-c", "cat > '"+path+"'")
	if err != nil {
		t.Fatalf("writing %s in %s: %v\n%s", path, service, err, out)
	}
}

// sha256In returns the hex sha256 of a file inside a container, or "" when the
// file does not exist.
func (s *stack) sha256In(service, path string) string {
	out, err := s.execErr(service, nil, "sha256sum", path)
	if err != nil {
		return ""
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// listDirIn returns the entry names of a directory inside a container,
// including dotfiles.
func (s *stack) listDirIn(t *testing.T, service, dir string) []string {
	t.Helper()
	out, err := s.execErr(service, nil, "sh", "-c", "ls -A '"+dir+"' 2>/dev/null || true")
	if err != nil {
		t.Fatalf("listing %s in %s: %v\n%s", dir, service, err, out)
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// coldstore HTTP client
// ---------------------------------------------------------------------------

// request issues an authenticated request against the containerised daemon. The
// caller closes the body.
func (s *stack) request(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()
	resp, err := s.requestErr(method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (s *stack) requestErr(method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, s.coldstore+path, reader)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(liveUsername, livePassword)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.http.Do(req)
}

// requestNoAuth is the same call with no Authorization header, for the tests
// that are about the credential gate itself.
func (s *stack) requestNoAuth(t *testing.T, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.coldstore+path, nil)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// mustJSON performs the request, insists on wantStatus, and decodes into out.
func (s *stack) mustJSON(t *testing.T, method, path string, body, out any, wantStatus int) {
	t.Helper()
	resp := s.request(t, method, path, body)
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading body: %v", method, path, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s: status %d, want %d\nbody: %s", method, path, resp.StatusCode, wantStatus, raw)
	}
	if out == nil {
		return
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s %s: decoding %T: %v\nbody: %s", method, path, out, err, raw)
	}
}

// Wire types. These are duplicated rather than imported from internal/api,
// which is the point: a live test that shared the server's structs would follow
// a breaking rename straight past the client that is going to break.
type liveStatus struct {
	TotalFiles           int64        `json:"total_files"`
	SyncedFiles          int64        `json:"synced_files"`
	ArchivedFiles        int64        `json:"archived_files"`
	MissingFiles         int64        `json:"missing_files"`
	ColdStorageFreeBytes uint64       `json:"cold_storage_free_bytes"`
	Folders              []liveFolder `json:"folders"`
}

type liveFolder struct {
	Label           string `json:"label"`
	SyncedSizeBytes int64  `json:"synced_size_bytes"`
	BudgetBytes     int64  `json:"budget_bytes"`
	PinnedBytes     int64  `json:"pinned_bytes"`
	OverBudget      bool   `json:"over_budget"`
	Wedged          bool   `json:"wedged"`
	GraceMinutes    int    `json:"grace_minutes"`
	Enabled         bool   `json:"enabled"`
	PresentInTOML   bool   `json:"present_in_toml"`
	LastEnforcedAt  *int64 `json:"last_enforced_at"`
	Counts          struct {
		Total    int64 `json:"total"`
		Synced   int64 `json:"synced"`
		Archived int64 `json:"archived"`
		Missing  int64 `json:"missing"`
		Pinned   int64 `json:"pinned"`
	} `json:"counts"`
}

type liveFolderConfig struct {
	FolderLabel   string `json:"folder_label"`
	SyncthingID   string `json:"syncthing_id"`
	SyncthingPath string `json:"syncthing_path"`
	BudgetBytes   int64  `json:"budget_bytes"`
	GraceMinutes  int    `json:"grace_minutes"`
	Enabled       bool   `json:"enabled"`
	PresentInTOML bool   `json:"present_in_toml"`
}

type liveFile struct {
	ID            int64  `json:"id"`
	FolderLabel   string `json:"folder_label"`
	RelativePath  string `json:"relative_path"`
	Filename      string `json:"filename"`
	SizeBytes     int64  `json:"size_bytes"`
	Status        string `json:"status"`
	Pinned        bool   `json:"pinned"`
	PinSource     string `json:"pin_source"`
	ThumbnailPath string `json:"thumbnail_path,omitempty"`
}

type liveJob struct {
	ID          int64  `json:"id"`
	Action      string `json:"action"`
	FolderLabel string `json:"folder_label"`
	State       string `json:"state"`
	Total       int    `json:"total"`
	Done        int    `json:"done"`
	FailedCount int    `json:"failed_count"`
	Error       string `json:"error"`
	Failed      []struct {
		ID    int64  `json:"id"`
		Error string `json:"error"`
	} `json:"failed"`
}

type liveJobCreated struct {
	JobID  int64  `json:"job_id"`
	Action string `json:"action"`
	State  string `json:"state"`
	Total  int    `json:"total"`
}

func (s *stack) status(t *testing.T) liveStatus {
	t.Helper()
	var out liveStatus
	s.mustJSON(t, http.MethodGet, "/api/v1/status", nil, &out, http.StatusOK)
	return out
}

func (s *stack) folder(t *testing.T, label string) liveFolder {
	t.Helper()
	for _, f := range s.status(t).Folders {
		if f.Label == label {
			return f
		}
	}
	t.Fatalf("no folder %q in GET /api/v1/status", label)
	return liveFolder{}
}

// fileByRelPath finds a catalog record by its path within the managed folder.
// It returns ok=false rather than failing, so callers can poll for it.
func (s *stack) fileByRelPath(t *testing.T, rel string) (liveFile, bool) {
	t.Helper()
	var files []liveFile
	s.mustJSON(t, http.MethodGet,
		"/api/v1/files?folder="+liveFolderLabel+"&limit=500", nil, &files, http.StatusOK)
	for _, f := range files {
		if f.RelativePath == rel {
			return f, true
		}
	}
	return liveFile{}, false
}

// scanAndWait triggers a rescan through the API and blocks until the resulting
// job reaches a terminal state. POST /scan is asynchronous (design 6.11), so a
// test that just posts and looks is racing the worker.
func (s *stack) scanAndWait(t *testing.T) {
	t.Helper()
	var created liveJobCreated
	s.mustJSON(t, http.MethodPost, "/api/v1/scan", nil, &created, http.StatusAccepted)
	job := s.waitForJob(t, created.JobID, 90*time.Second)
	if job.State != "done" {
		t.Fatalf("scan job %d finished in state %q (error %q)", job.ID, job.State, job.Error)
	}
}

func (s *stack) job(t *testing.T, id int64) liveJob {
	t.Helper()
	var out liveJob
	s.mustJSON(t, http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d", id), nil, &out, http.StatusOK)
	return out
}

func (s *stack) waitForJob(t *testing.T, id int64, timeout time.Duration) liveJob {
	t.Helper()
	var last liveJob
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		last = s.job(t, id)
		if last.State == "done" || last.State == "failed" {
			return last
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("job %d still %q (%d/%d) after %s", id, last.State, last.Done, last.Total, timeout)
	return last
}

// awaitFile polls a catalog record until pred is happy. Every state change in
// this system is eventually-consistent across at least two daemons, so polling
// with a deadline is the only honest way to assert on it.
func (s *stack) awaitFile(t *testing.T, rel string, timeout time.Duration, what string, pred func(liveFile) bool) liveFile {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last liveFile
	var found bool
	for time.Now().Before(deadline) {
		last, found = s.fileByRelPath(t, rel)
		if found && pred(last) {
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !found {
		t.Fatalf("timed out after %s waiting for %s: no catalog record for %q", timeout, what, rel)
	}
	t.Fatalf("timed out after %s waiting for %s: %q is status=%s pinned=%v",
		timeout, what, rel, last.Status, last.Pinned)
	return last
}

// await polls an arbitrary condition. It reports the failure in terms of what
// the caller was waiting for, because "condition false" is useless at 3am.
func await(t *testing.T, timeout time.Duration, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// awaitNoFatal is await for conditions a caller wants to observe rather than
// require. It reports whether the condition held before the deadline.
func awaitNoFatal(timeout time.Duration, pred func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// ---------------------------------------------------------------------------
// Syncthing REST client
// ---------------------------------------------------------------------------

type syncthingClient struct {
	name   string
	base   string
	apiKey string
	http   *http.Client
}

func (c *syncthingClient) do(method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

func (c *syncthingClient) mustDo(method, path string, body, out any) error {
	code, raw, err := c.do(method, path, body)
	if err != nil {
		return fmt.Errorf("%s %s %s: %w", c.name, method, path, err)
	}
	if code < 200 || code > 299 {
		return fmt.Errorf("%s %s %s: status %d: %s", c.name, method, path, code, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s %s: decoding: %w: %s", c.name, method, path, err, raw)
	}
	return nil
}

func (c *syncthingClient) deviceID() (string, error) {
	var out struct {
		MyID string `json:"myID"`
	}
	if err := c.mustDo(http.MethodGet, "/rest/system/status", nil, &out); err != nil {
		return "", err
	}
	if out.MyID == "" {
		return "", fmt.Errorf("%s: /rest/system/status returned an empty myID", c.name)
	}
	return out.MyID, nil
}

func (c *syncthingClient) connectedTo(deviceID string) bool {
	var out struct {
		Connections map[string]struct {
			Connected bool `json:"connected"`
		} `json:"connections"`
	}
	if err := c.mustDo(http.MethodGet, "/rest/system/connections", nil, &out); err != nil {
		return false
	}
	conn, ok := out.Connections[deviceID]
	return ok && conn.Connected
}

// settled reports whether this device has nothing left to do on the folder: it
// is not scanning, not pulling, needs no file and no deletion, and its local
// index holds every file the cluster's global index does.
//
// "The file is on disk over there" is NOT the same statement. Syncthing writes a
// pulled file and records it in its own index as two steps, and its scanner runs
// alongside (rescanIntervalS=10, plus the 1 s fs watcher). A scan that lands in
// that gap sees a file its index does not fully account for and bumps the
// version, which turns a subsequent deletion from the other device into a
// concurrent-change conflict instead of a successor -- the file is sent back,
// the archived path is "occupied again" on every scan, and the rest of the run
// is polluted. Quiescence on both devices is what closes that window.
func (c *syncthingClient) settled(folderID string) bool {
	var out struct {
		State       string `json:"state"`
		NeedFiles   int    `json:"needFiles"`
		NeedDeletes int    `json:"needDeletes"`
		GlobalFiles int    `json:"globalFiles"`
		LocalFiles  int    `json:"localFiles"`
	}
	if err := c.mustDo(http.MethodGet, "/rest/db/status?folder="+folderID, nil, &out); err != nil {
		return false
	}
	return out.State == "idle" &&
		out.NeedFiles == 0 &&
		out.NeedDeletes == 0 &&
		out.LocalFiles == out.GlobalFiles
}

// indexEntry is the slice of /rest/db/file this suite cares about. A 404 means
// the name has never been in this device's index at all -- which is a different
// and much stronger statement than "the file is not on disk".
type indexEntry struct {
	Present bool
	Deleted bool
	Size    int64
}

func (c *syncthingClient) indexEntry(folderID, name string) (indexEntry, error) {
	code, raw, err := c.do(http.MethodGet,
		"/rest/db/file?folder="+folderID+"&file="+name, nil)
	if err != nil {
		return indexEntry{}, err
	}
	if code == http.StatusNotFound {
		return indexEntry{Present: false}, nil
	}
	if code != http.StatusOK {
		return indexEntry{}, fmt.Errorf("%s: /rest/db/file %s: status %d: %s", c.name, name, code, raw)
	}
	var out struct {
		Global struct {
			Deleted bool  `json:"deleted"`
			Size    int64 `json:"size"`
		} `json:"global"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return indexEntry{}, fmt.Errorf("%s: decoding /rest/db/file: %w", c.name, err)
	}
	return indexEntry{Present: true, Deleted: out.Global.Deleted, Size: out.Global.Size}, nil
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

// portFree reports whether a TCP port on loopback can be bound right now. The
// stack publishes three ports; a collision produces an unreadable compose error
// several seconds later, so it is worth naming the culprit up front.
func portFree(port string) bool {
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// randomBytes builds deterministic-length, non-compressible content. Zero-filled
// files would let a filesystem or Syncthing block-dedupe make a "the bytes came
// back" assertion vacuous.
func randomBytes(n int, seed byte) []byte {
	b := make([]byte, n)
	x := seed | 1
	for i := range b {
		x = x*167 + 13
		b[i] = x
	}
	return b
}
