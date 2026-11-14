package archivetier

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/fsx"
	"github.com/tjmisko/coldstore/internal/fsx/fsxtest"
)

type receiptOutcome struct {
	Status int
	Body   string
}

// resendAll sends every file again, in order, through an in-process server and
// asks for a receipt over all of them.
func resendAll(t *testing.T, root string, files []sourceFile) (items int, receipt receiptOutcome) {
	t.Helper()
	service := openService(t, serviceOptions(root))
	ts := newAPIServer(t, root, service)
	for _, file := range files {
		mustPutFile(t, ts.URL, file)
	}
	items = countItems(t, service.Index())

	var hashes []string
	for _, file := range files {
		hashes = append(hashes, `"`+hashHex(file.Body)+`"`)
	}
	resp, err := http.Post(ts.URL+"/api/v1/collections/"+collectionName+"/receipt", "application/json",
		strings.NewReader("["+strings.Join(hashes, ",")+"]"))
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the receipt: %v", err)
	}
	closeService(t, service)
	return items, receiptOutcome{Status: resp.StatusCode, Body: string(raw)}
}

func TestCrashUpload(t *testing.T) {
	files := demoSource()
	for _, point := range matrixPoints(t) {
		for _, hit := range hitsFor(point) {
			t.Run(fmt.Sprintf("%s hit %d", point, hit), func(t *testing.T) {
				root := prepareRoot(t, point)
				baseline := baselineItems(t, root)

				run := runUploadChild(t, point, hit, root, files)
				if !run.Died {
					t.Fatalf("the upload child was not killed at %s hit %d (exit: %v): the point was never reached\n%s",
						point, hit, run.Exit, run.Stderr)
				}

				t.Run(fmt.Sprintf("should verify every journaled blob when the daemon dies at %s hit %d during an upload", point, hit),
					func(t *testing.T) { checkInvariants(t, root, nil) })

				t.Run(fmt.Sprintf("should accept the same uploads with no duplicate items when the client retries after a crash at %s hit %d", point, hit),
					func(t *testing.T) {
						items, receipt := resendAll(t, root, files)
						if want := baseline + distinctContents(files); items != want {
							t.Fatalf("items = %d, want %d: a crash left a duplicate or a lost Item", items, want)
						}
						checkInvariants(t, root, nil)

						t.Run("should issue a full receipt when the retried batch completes", func(t *testing.T) {
							if receipt.Status != http.StatusOK || !strings.Contains(receipt.Body, `"all_verified":true`) {
								t.Fatalf("receipt = %d %s", receipt.Status, receipt.Body)
							}
						})
					})
			})
		}
	}
}

func TestInjectedFaultsEndToEnd(t *testing.T) {
	stagingFault := func(root string, op fsxtest.Op, err error, short bool) fsxtest.Fault {
		return fsxtest.Fault{Op: op, PathContains: filepath.Join(root, "tmp") + string(filepath.Separator), Nth: 1, Err: err, Short: short}
	}
	cases := []struct {
		name       string
		fault      func(root string) fsxtest.Fault
		wantStatus int // 0 = any non-2xx
	}{
		{"EIO on write", func(root string) fsxtest.Fault { return stagingFault(root, fsxtest.OpWrite, syscall.EIO, false) }, 0},
		{"a short write", func(root string) fsxtest.Fault { return stagingFault(root, fsxtest.OpWrite, nil, true) }, 0},
		{"a failing fsync", func(root string) fsxtest.Fault { return stagingFault(root, fsxtest.OpSync, syscall.EIO, false) }, 0},
		{"ENOSPC on write", func(root string) fsxtest.Fault { return stagingFault(root, fsxtest.OpWrite, syscall.ENOSPC, false) }, http.StatusInsufficientStorage},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("should answer non-2xx, link nothing and record ingest_failed when an upload meets %s, then succeed once the fault clears", tc.name), func(t *testing.T) {
			root := newRoot(t)
			faulty := fsxtest.NewFaulty(fsx.OS())
			opts := serviceOptions(root)
			opts.FS = faulty
			service := openService(t, opts)
			ts := newAPIServer(t, root, service)
			file := demoSource()[0]
			hash := parseHash(t, hashHex(file.Body))

			faulty.Inject(tc.fault(root))
			status, body, err := putFile(&http.Client{Timeout: 20 * time.Second}, ts.URL, collectionName, file)
			if err != nil {
				t.Fatalf("PUT: %v", err)
			}
			if status >= 200 && status < 300 {
				t.Fatalf("PUT = %d %s during the fault, want non-2xx", status, body)
			}
			if tc.wantStatus != 0 && status != tc.wantStatus {
				t.Fatalf("PUT = %d %s, want %d", status, body, tc.wantStatus)
			}
			if _, held, err := service.Store().Has(hash); err != nil || held {
				t.Errorf("the blob is linked under its hash after a failed upload (held=%v, err=%v)", held, err)
			}
			if _, err := os.Stat(service.Store().Path(hash)); err == nil {
				t.Errorf("a file exists at %s after a failed upload", service.Store().Path(hash))
			}
			events, err := service.Index().Events("", "ingest_failed", 10)
			if err != nil || len(events) == 0 {
				t.Errorf("no ingest_failed event (err=%v)", err)
			}

			faulty.Reset()
			mustPutFile(t, ts.URL, file)
			if _, held, err := service.Store().Has(hash); err != nil || !held {
				t.Errorf("the re-sent upload is not stored (held=%v, err=%v)", held, err)
			}
		})
	}
}
