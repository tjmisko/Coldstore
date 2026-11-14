package archivetier

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/api"
	"github.com/tjmisko/coldstore/internal/archive"
	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/scrub"
)

const corruptionFileCount = 30

type damageSet struct {
	Flipped, Truncated, Deleted index.Entry
}

func (d damageSet) all() []index.Entry { return []index.Entry{d.Flipped, d.Truncated, d.Deleted} }

type corruptionFixture struct {
	Service *archive.Service
	Server  *httptest.Server
	Files   map[string]string // path -> body
	Entries map[string]index.Entry
	Damage  damageSet
	// BeforeScrub is every damaged entry's download, taken before the scrub.
	BeforeScrub map[string]download
}

type download struct {
	Status  int
	Body    string
	ReadErr error
}

func (d download) intact(want string) bool {
	return d.Status == http.StatusOK && d.ReadErr == nil && d.Body == want
}

func fetchDownload(t *testing.T, ts *httptest.Server, entry index.Entry) download {
	t.Helper()
	url := fmt.Sprintf("%s/api/v1/files/%d/download", ts.URL, api.CollectionIDBase+entry.EntryID)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Get(url)
	if err != nil {
		// The handler aborted the connection before sending headers: a failed
		// download, which is exactly what a damaged blob must produce.
		return download{ReadErr: err}
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	return download{Status: resp.StatusCode, Body: string(raw), ReadErr: readErr}
}

func headBlob(t *testing.T, ts *httptest.Server, hash string) int {
	t.Helper()
	resp, err := http.Head(ts.URL + "/api/v1/blobs/" + hash)
	if err != nil {
		t.Fatalf("HEAD %s: %v", hash, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// newCorruptionFixture imports 30 distinct files, then flips a byte in one
// blob, truncates a second and deletes a third. It downloads the damaged files
// once before any scrub.
func newCorruptionFixture(t *testing.T) *corruptionFixture {
	t.Helper()
	root := newRoot(t)
	service := openService(t, serviceOptions(root))
	fixture := &corruptionFixture{
		Service: service, Server: newAPIServer(t, root, service),
		Files: map[string]string{}, Entries: map[string]index.Entry{}, BeforeScrub: map[string]download{},
	}

	var files []sourceFile
	for i := 0; i < corruptionFileCount; i++ {
		file := sourceFile{
			Path: fmt.Sprintf("Batch/%02d/scan-%02d.txt", i%5, i),
			Body: fmt.Sprintf("scan number %02d\n%s", i, strings.Repeat(fmt.Sprintf("line %02d of padding\n", i), 20)),
		}
		files = append(files, file)
		fixture.Files[file.Path] = file.Body
	}
	source := writeSource(t, files)
	id, err := service.ImportStart(collectionName, source)
	if err != nil {
		t.Fatalf("ImportStart: %v", err)
	}
	if status := waitForImport(t, service, id); status.State != "done" || status.FailedCount != 0 {
		t.Fatalf("import: %+v", status)
	}

	for _, entry := range allEntries(t, service.Index(), nil) {
		fixture.Entries[entry.Path] = entry
	}
	if len(fixture.Entries) != corruptionFileCount {
		t.Fatalf("imported %d entries, want %d", len(fixture.Entries), corruptionFileCount)
	}
	fixture.Damage = damageSet{
		Flipped:   fixture.Entries["Batch/00/scan-00.txt"],
		Truncated: fixture.Entries["Batch/01/scan-11.txt"],
		Deleted:   fixture.Entries["Batch/02/scan-22.txt"],
	}
	damageBlobs(t, service, fixture.Damage)
	for _, entry := range fixture.Damage.all() {
		fixture.BeforeScrub[entry.Path] = fetchDownload(t, fixture.Server, entry)
	}
	return fixture
}

func damageBlobs(t *testing.T, service *archive.Service, damage damageSet) {
	t.Helper()
	// Blobs are stored read-only; bit rot does not care.
	for _, entry := range damage.all() {
		if err := os.Chmod(service.Store().Path(entry.Hash), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	flipped := service.Store().Path(damage.Flipped.Hash)
	raw, err := os.ReadFile(flipped)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(flipped, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(service.Store().Path(damage.Truncated.Hash), 40); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(service.Store().Path(damage.Deleted.Hash)); err != nil {
		t.Fatal(err)
	}
}

func runScrub(t *testing.T, service *archive.Service) scrub.Report {
	t.Helper()
	if err := service.ScrubStart(0); err != nil {
		t.Fatalf("ScrubStart: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		status := service.ScrubStatus()
		if !status.Running && status.Last != nil {
			return *status.Last
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the scrub did not finish")
	return scrub.Report{}
}

func TestCorruptionAndScrub(t *testing.T) {
	// One fixture and one scrub for the four assertions, in the order the
	// design states them: before the scrub, the scrub, after the scrub.
	fixture := newCorruptionFixture(t)

	t.Run("should fail the download of each damaged blob before the scrub reports it", func(t *testing.T) {
		for _, entry := range fixture.Damage.all() {
			got := fixture.BeforeScrub[entry.Path]
			if got.intact(fixture.Files[entry.Path]) {
				t.Errorf("%s: downloaded intact although its blob is damaged", entry.Path)
			}
			if got.Status == http.StatusOK && got.ReadErr == nil {
				t.Errorf("%s: 200 with a complete body (%d bytes) from a damaged blob", entry.Path, len(got.Body))
			}
		}
	})

	report := runScrub(t, fixture.Service)

	t.Run("should report exactly the flipped, truncated and deleted blobs when scrubbed", func(t *testing.T) {
		var got, want []string
		for _, finding := range report.Findings {
			got = append(got, finding.Hash.String()+" "+finding.Kind+" "+finding.Reason)
		}
		reasons := map[string]string{
			fixture.Damage.Flipped.Hash.String():   "hash_mismatch",
			fixture.Damage.Truncated.Hash.String(): "truncated",
			fixture.Damage.Deleted.Hash.String():   "missing",
		}
		for hash, reason := range reasons {
			want = append(want, hash+" bad "+reason)
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if report.Checked != corruptionFileCount {
			t.Errorf("checked %d blobs, want %d", report.Checked, corruptionFileCount)
		}
	})

	t.Run("should fail the download of each damaged blob", func(t *testing.T) {
		for _, entry := range fixture.Damage.all() {
			got := fetchDownload(t, fixture.Server, entry)
			if got.Status != http.StatusServiceUnavailable {
				t.Errorf("%s: download after the scrub = %d (read error %v), want 503", entry.Path, got.Status, got.ReadErr)
			}
			if got.Body != "" && got.Body == fixture.Files[entry.Path] {
				t.Errorf("%s: the damaged blob's bytes were served", entry.Path)
			}
			if status := headBlob(t, fixture.Server, entry.Hash.String()); status != http.StatusNotFound {
				t.Errorf("%s: HEAD /blobs = %d, want 404", entry.Path, status)
			}
		}
	})

	t.Run("should download every undamaged file byte-identical", func(t *testing.T) {
		damaged := map[string]bool{}
		for _, entry := range fixture.Damage.all() {
			damaged[entry.Path] = true
		}
		checked := 0
		for path, entry := range fixture.Entries {
			if damaged[path] {
				continue
			}
			checked++
			if got := fetchDownload(t, fixture.Server, entry); !got.intact(fixture.Files[path]) {
				t.Errorf("%s: status %d, read error %v, %d bytes: not byte-identical", path, got.Status, got.ReadErr, len(got.Body))
			}
			if status := headBlob(t, fixture.Server, entry.Hash.String()); status != http.StatusOK {
				t.Errorf("%s: HEAD /blobs = %d, want 200", path, status)
			}
		}
		if checked != corruptionFileCount-3 {
			t.Errorf("checked %d undamaged files, want %d", checked, corruptionFileCount-3)
		}
	})

	t.Run("should flag nothing else", func(t *testing.T) {
		stats, err := fixture.Service.Index().ArchiveStats()
		if err != nil {
			t.Fatal(err)
		}
		if stats.BadBlobCount != 3 {
			t.Errorf("bad blobs = %d, want 3", stats.BadBlobCount)
		}
		if len(report.Stray) != 0 {
			t.Errorf("stray files reported: %v", report.Stray)
		}
		for _, finding := range report.Findings {
			if finding.Kind == "orphan" {
				t.Errorf("orphan reported: %+v", finding)
			}
		}
		events, err := fixture.Service.Index().Events("", "blob_bad", 100)
		if err != nil {
			t.Fatal(err)
		}
		// A download and the scrub may each record the same blob; what matters
		// is that no other blob is ever named.
		damagedHashes := []string{}
		for _, entry := range fixture.Damage.all() {
			damagedHashes = append(damagedHashes, entry.Hash.String())
		}
		for _, event := range events {
			if !containsAny(event.Details, damagedHashes) {
				t.Errorf("a blob_bad event names no damaged blob: %q", event.Details)
			}
		}
	})
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}
