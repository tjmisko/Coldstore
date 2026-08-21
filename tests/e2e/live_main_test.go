//go:build e2e_live

package e2e

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain owns the containers.
//
// It builds the binary the image copies, brings the three-container stack up,
// pairs the two Syncthing devices over their REST APIs, waits until the shared
// folder is genuinely live on both, and tears everything down afterwards -- on
// success, on failure, and on a panic.
//
// Bringing the stack up HERE rather than in the Makefile is deliberate. The old
// arrangement had `make test-e2e-live` run `docker compose up` and then run a
// test binary that never opened a socket to it, so the two halves could not
// disagree and nothing noticed. With the lifecycle inside the suite, a plain
// `go test -tags e2e_live ./tests/e2e/...` is a complete, honest run, and the
// Makefile target is a one-line wrapper that cannot drift away from it.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) (code int) {
	root, err := filepath.Abs("../..")
	if err != nil {
		log.Printf("live: resolving repo root: %v", err)
		return 1
	}

	composeFile := envOr("COLDSTORE_LIVE_COMPOSE", filepath.Join("docker", "compose.test.yml"))
	coldstorePort := envOr("COLDSTORE_TEST_PORT", "18090")
	portA := envOr("SYNCTHING_A_PORT", "18384")
	portB := envOr("SYNCTHING_B_PORT", "18385")
	apiKey := envOr("SYNCTHING_API_KEY", "coldstore-live-test-key")

	live = stack{
		repoRoot:    root,
		composeFile: composeFile,
		coldstore:   "http://127.0.0.1:" + coldstorePort,
		http:        &http.Client{Timeout: 30 * time.Second},
		syncthingA: &syncthingClient{
			name: serviceSyncthingA, base: "http://127.0.0.1:" + portA,
			apiKey: apiKey, http: &http.Client{Timeout: 15 * time.Second},
		},
		syncthingB: &syncthingClient{
			name: serviceSyncthingB, base: "http://127.0.0.1:" + portB,
			apiKey: apiKey, http: &http.Client{Timeout: 15 * time.Second},
		},
	}

	if !envTrue("COLDSTORE_LIVE_SKIP_UP") {
		for name, port := range map[string]string{
			"COLDSTORE_TEST_PORT": coldstorePort,
			"SYNCTHING_A_PORT":    portA,
			"SYNCTHING_B_PORT":    portB,
		} {
			if portFree(port) {
				continue
			}
			// Far and away the likeliest cause is this suite's own leftovers.
			// The teardown below is a `defer`; a `go test -timeout` expiry
			// panics from the runtime's watchdog goroutine, which terminates
			// the process without unwinding it, so that run leaks three
			// containers, three volumes and these three ports. Telling that
			// operator to pick a different port sends them to build a second
			// leaked stack next to the first.
			if leaked := leakedStackContainers(); leaked != "" {
				log.Printf("live: port %s is in use and project %q still has containers up "+
					"(%s) -- a previous run was killed before its teardown ran. "+
					"Run `make test-e2e-live-down`.", port, composeProject, leaked)
				return 1
			}
			log.Printf("live: port %s is already in use by something that is not this suite; "+
				"set %s to a free port", port, name)
			return 1
		}

		if err := buildBinary(root); err != nil {
			log.Printf("live: %v", err)
			return 1
		}

		defer func() {
			if code != 0 {
				dumpLogs()
			}
			teardown()
		}()
		teardownOnInterrupt()

		if err := bringUp(); err != nil {
			log.Printf("live: %v", err)
			return 1
		}
	}

	// COLDSTORE_LIVE_SKIP_PAIR exists so the suite can be pointed at a
	// deliberately misconfigured stack. Pairing is idempotent and rewrites the
	// folder definition on both devices, so without this an injected fault --
	// flipping the folder to receiveonly, say -- would be repaired before the
	// first test ran, and the suite would prove it is incapable of going red.
	if !envTrue("COLDSTORE_LIVE_SKIP_PAIR") {
		if err := pairDevices(); err != nil {
			log.Printf("live: pairing the two Syncthing devices: %v", err)
			return 1
		}
	}

	return m.Run()
}

// buildBinary produces the binary docker/Dockerfile.test COPYs into the image.
// It is a plain `go build`, not `make build`: the live suite asserts nothing
// about the SPA, and requiring a working npm toolchain to run an integration
// test is a reason not to run it.
func buildBinary(root string) error {
	if envTrue("COLDSTORE_LIVE_SKIP_BUILD") {
		log.Print("live: COLDSTORE_LIVE_SKIP_BUILD set, using the existing ./coldstore binary")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	log.Print("live: building ./coldstore for the test image")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", "coldstore", "./cmd/coldstore")
	cmd.Dir = root
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("building the coldstore binary: %w\n%s", err, out)
	}
	return nil
}

func bringUp() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	log.Print("live: docker compose up --build --wait")
	// --wait blocks on the healthchecks, so a container that starts and then
	// exits 1 fails here instead of producing a suite of connection refusals.
	out, err := live.composeRun(ctx, "up", "-d", "--build", "--wait", "--remove-orphans")
	if err != nil {
		return fmt.Errorf("bringing the stack up: %w\n%s", err, out)
	}
	return nil
}

// teardownOnInterrupt covers the half of the killed-run problem that can be
// covered.
//
// The deferred teardown above only runs when run() RETURNS. Ctrl-C does not
// unwind it, and neither does `go test -timeout` expiry -- that one panics from
// the watchdog goroutine, which terminates the process without running any other
// goroutine's defers, and no handler can intercept it. Ctrl-C can be caught, so
// it is; the timeout case is why the port pre-flight above knows how to spot a
// leaked stack and name `make test-e2e-live-down`.
func teardownOnInterrupt() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sig
		log.Printf("live: caught %s, tearing the stack down before exiting", s)
		teardown()
		os.Exit(130)
	}()
}

// teardownOnce keeps the signal handler and the deferred teardown from running
// `docker compose down` concurrently, which produces an unreadable pair of
// errors rather than a clean stop.
var teardownOnce sync.Once

func teardown() {
	teardownOnce.Do(doTeardown)
}

func doTeardown() {
	if envTrue("COLDSTORE_LIVE_KEEP") {
		log.Printf("live: COLDSTORE_LIVE_KEEP set, leaving project %q running", composeProject)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	log.Print("live: docker compose down -v")
	if out, err := live.composeRun(ctx, "down", "-v", "--remove-orphans"); err != nil {
		// Nothing to fail: the tests have already run. But a leaked volume
		// poisons the next run, so it must be loud.
		log.Printf("live: TEARDOWN FAILED, containers or volumes may be left behind: %v\n%s", err, out)
	}
}

// leakedStackContainers names the containers this suite's compose project still
// has up, or "" when the project is clean or docker cannot answer.
//
// It exists so the port pre-flight can tell "somebody else is on 18090" apart
// from "your last run was killed", which have completely different fixes.
func leakedStackContainers() string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := live.composeRun(ctx, "ps", "--format", "{{.Name}}")
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(out), " ")
}

func dumpLogs() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := live.composeRun(ctx, "logs", "--no-color", "--tail", "200")
	if err != nil {
		log.Printf("live: could not collect container logs: %v", err)
	}
	log.Printf("live: container logs follow\n%s", out)
}

// ---------------------------------------------------------------------------
// pairing
// ---------------------------------------------------------------------------

// pairDevices turns two independent Syncthing daemons into a pair sharing one
// folder. There is no way to bake this into an image: a device ID is the hash of
// a keypair the container generates on first start, so each side's id is only
// knowable at run time.
func pairDevices() error {
	idA, err := live.syncthingA.deviceID()
	if err != nil {
		return err
	}
	idB, err := live.syncthingB.deviceID()
	if err != nil {
		return err
	}
	log.Printf("live: device A = %s", idA)
	log.Printf("live: device B = %s", idB)

	// Hermetic: no global discovery, no relays, no NAT traversal, no usage
	// reporting. The two containers find each other by the static address
	// below and nothing in this suite depends on the public internet.
	options := map[string]any{
		"globalAnnounceEnabled": false,
		"relaysEnabled":         false,
		"natEnabled":            false,
		"localAnnounceEnabled":  false,
		"urAccepted":            -1,
	}
	for _, c := range []*syncthingClient{live.syncthingA, live.syncthingB} {
		if err := c.mustDo(http.MethodPatch, "/rest/config/options", options, nil); err != nil {
			return err
		}
	}

	// PUT, not POST: PUT is an upsert, so a re-run against an already-paired
	// stack (COLDSTORE_LIVE_SKIP_UP=1) converges instead of erroring.
	err = live.syncthingA.mustDo(http.MethodPut, "/rest/config/devices/"+idB, map[string]any{
		"deviceID":  idB,
		"name":      "device-b",
		"addresses": []string{"tcp://" + serviceSyncthingB + ":22000"},
	}, nil)
	if err != nil {
		return err
	}
	err = live.syncthingB.mustDo(http.MethodPut, "/rest/config/devices/"+idA, map[string]any{
		"deviceID":  idA,
		"name":      "device-a",
		"addresses": []string{"tcp://" + serviceSyncthingA + ":22000"},
	}, nil)
	if err != nil {
		return err
	}

	folder := map[string]any{
		"id":    liveSyncthingFolderID,
		"label": "LiveSync",
		"path":  syncthingSyncPath,
		// sendreceive is load-bearing. On a receiveonly folder -- Syncthing's own
		// recommendation for a backup target -- coldstore's deletions are never
		// propagated and the entire product is silently void (design 3.7). If
		// that is ever changed here, the propagation tests stop meaning anything.
		"type": "sendreceive",
		// Short so the suite does not spend minutes waiting on a rescan when the
		// inotify watcher misses something.
		"rescanIntervalS":  10,
		"fsWatcherEnabled": true,
		"fsWatcherDelayS":  1,
		"devices": []map[string]string{
			{"deviceID": idA},
			{"deviceID": idB},
		},
	}
	for _, c := range []*syncthingClient{live.syncthingA, live.syncthingB} {
		if err := c.mustDo(http.MethodPut,
			"/rest/config/folders/"+liveSyncthingFolderID, folder, nil); err != nil {
			return err
		}
	}

	if err := waitUntil(90*time.Second, "the two devices to connect", func() bool {
		return live.syncthingA.connectedTo(idB) && live.syncthingB.connectedTo(idA)
	}); err != nil {
		return err
	}

	// .stfolder is created by Syncthing, not by the image (see the note in
	// docker/Dockerfile.test). Until it exists, coldstore correctly treats the
	// managed folder as an unmounted volume and refuses to move anything
	// (design 3.6), so every archive assertion in the suite would fail for a
	// reason that has nothing to do with what it is testing.
	if err := waitUntil(60*time.Second, "Syncthing to create the .stfolder marker on both devices", func() bool {
		return live.isDirIn(serviceColdstore, coldstoreSyncPath+"/.stfolder") &&
			live.isDirIn(serviceSyncthingB, syncthingSyncPath+"/.stfolder")
	}); err != nil {
		return err
	}

	// And finally: the daemon has to answer. --wait already proved the health
	// check passed, but the health check runs inside the container; this is the
	// first request over the published port, which is what every test uses.
	return waitUntil(60*time.Second, "the coldstore API to answer on the published port", func() bool {
		resp, err := live.requestErr(http.MethodGet, "/api/v1/status", nil)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

// waitUntil is await for setup code, which has no *testing.T to fail.
func waitUntil(timeout time.Duration, what string, pred func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
}
