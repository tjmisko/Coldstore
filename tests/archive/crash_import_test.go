package archivetier

import (
	"fmt"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/archive"
)

// rerunImport opens the root in-process and imports the whole source again.
func rerunImport(t *testing.T, root string, source *sourceTree) {
	t.Helper()
	service := openService(t, serviceOptions(root))
	id, err := service.ImportStart(collectionName, source.Dir)
	if err != nil {
		t.Fatalf("ImportStart: %v", err)
	}
	status := waitForImport(t, service, id)
	if status.State != "done" || status.FailedCount != 0 {
		t.Fatalf("the re-run finished %q with %d failures: %+v", status.State, status.FailedCount, status.Failed)
	}
	closeService(t, service)
}

func waitForImport(t *testing.T, service *archive.Service, id int64) archive.ImportStatus {
	t.Helper()
	for range 20000 {
		status, ok := service.ImportStatus(id)
		if !ok {
			t.Fatalf("import %d is unknown", id)
		}
		if status.State != "running" {
			return status
		}
		sleepBriefly()
	}
	t.Fatalf("import %d did not finish", id)
	return archive.ImportStatus{}
}

func sleepBriefly() { time.Sleep(time.Millisecond) }

func TestCrashImport(t *testing.T) {
	files := demoSource()
	for _, point := range matrixPoints(t) {
		for _, hit := range hitsFor(point) {
			t.Run(fmt.Sprintf("%s hit %d", point, hit), func(t *testing.T) {
				root := prepareRoot(t, point)
				baseline := baselineItems(t, root)
				source := newSourceTree(t, files)

				run := runImportChild(t, point, hit, root, source.Dir)
				if !run.Died {
					t.Fatalf("the import child was not killed at %s hit %d (exit: %v): the point was never reached\n%s",
						point, hit, run.Exit, run.Stderr)
				}

				t.Run(fmt.Sprintf("should verify every journaled blob and leave the source untouched when the import dies at %s hit %d", point, hit),
					func(t *testing.T) { checkInvariants(t, root, source) })

				t.Run(fmt.Sprintf("should finish with no duplicate items when the import is re-run after dying at %s hit %d", point, hit),
					func(t *testing.T) {
						rerunImport(t, root, source)
						service := openService(t, serviceOptions(root))
						items := countItems(t, service.Index())
						placements := len(allEntries(t, service.Index(), nil))
						closeService(t, service)
						if want := baseline + distinctContents(files); items != want {
							t.Fatalf("items = %d, want %d: a crash left a duplicate or a lost Item", items, want)
						}
						if placements < len(files) {
							t.Fatalf("placements = %d, want at least %d", placements, len(files))
						}
						checkInvariants(t, root, source)
					})
			})
		}
	}
}

func TestCrashImportCoverage(t *testing.T) {
	t.Run("should reach every registered crash point during the import scenario", func(t *testing.T) {
		files := demoSource()
		for _, point := range matrixPoints(t) {
			root := prepareRoot(t, point)
			source := newSourceTree(t, files)
			run := runImportChild(t, point, 1, root, source.Dir)
			if !run.Died {
				t.Errorf("crash point %s was never reached by the import scenario (exit: %v)\n%s", point, run.Exit, run.Stderr)
			}
		}
	})
}
