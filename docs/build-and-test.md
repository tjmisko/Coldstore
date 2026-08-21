# Build Pipeline and Testing Guide

## Makefile Targets

| Target | What it does | When to use it |
|---|---|---|
| `deps` | `npm ci` in `web/`, skipped when `web/node_modules` already exists | Never called by hand; it is a prerequisite of `frontend` so a fresh clone builds |
| `frontend` | `deps`, then `npm run build` in `web/`, then copies `web/dist` to `internal/api/dist/` | After changing React code, before building the binary |
| `build` | Runs `frontend`, then `go build -o coldstore ./cmd/coldstore` | Produce a release binary with embedded frontend |
| `binary` | `go build -o coldstore ./cmd/coldstore` with no frontend step | A binary for the test image, without needing npm |
| `dev` | `go run ./cmd/coldstore --config coldstore.toml` | Run the Go daemon locally during development |
| `dev-web` | `cd web && npm run dev` (Vite dev server) | Run the React frontend with hot reload |
| `lint` | `cd web && npm run lint` (ESLint) | Check the React code before pushing; CI runs the same command |
| `test` | `go test ./...` | Run Go unit tests (no build tags) |
| `test-web` | `cd web && npm run test` (vitest) | Run the React unit and component tests: jsdom, stubbed `fetch`, no daemon and no browser |
| `test-e2e` | `go test -tags e2e -count=1 -timeout 120s ./tests/e2e/...` | In-process e2e against real SQLite + temp dirs. No Docker, no Syncthing, no network |
| `test-e2e-live` | `go test -tags e2e_live -count=1 -timeout 1800s -v ./tests/e2e/...`; the suite's own `TestMain` builds the binary, brings up two Syncthing devices plus the containerised daemon, pairs them, and tears down | Prove the things only real Syncthing can prove: that an offload propagates as a deletion, and a restore propagates back |
| `test-e2e-live-down` | `docker compose -p coldstore-live -f docker/compose.test.yml down -v` | Clean up after `COLDSTORE_LIVE_KEEP=1` or an interrupted run |
| `test-playwright` | `cd web && npx playwright test` | Drive the embedded UI in a real browser against a real daemon over a freshly seeded demo installation |
| `demo` | `go run ./scripts/seed-demo`, then run the binary against it | A complete demo installation on `http://127.0.0.1:8090`, covering every state the control plane renders |
| `node-modules-boundary` | Writes a stub `go.mod` into `web/node_modules` | Never called by hand; `deps` runs it so `go build ./...` does not walk into npm packages that ship Go source |
| `clean` | Remove the `coldstore` binary and the built frontend, then restore the tracked `internal/api/dist/.gitkeep` stub | Reset build artifacts |
| `install-binary` | `install -m 755 ./coldstore /usr/local/bin/coldstore`, with no build step | The runbook's install step: `make build && sudo make install-binary` |
| `install` | Run `build`, then `install-binary` | Convenient, but **not** under `sudo`: it builds as root and leaves root-owned `web/dist/`, `internal/api/dist/` and `./coldstore` behind, which breaks the next unprivileged `make build` |

Source: `Makefile`

The offline status path is covered by `internal/catalog` (immutable/query-only opening),
`internal/diagnostic` (health classification, redaction and rendering), and
`cmd/coldstore` (flag/exit behavior). These are ordinary `go test ./...` tests; they do
not need a daemon, network, root, btrfs hardware or Syncthing.

## Build Pipeline Walkthrough

`make build` produces a single self-contained binary in five steps:

0. **npm ci** -- only when `web/node_modules` is missing. It is gitignored, so this is the
   state every fresh clone (and the deployment runbook's first build) starts in.
1. **npm build** -- `cd web && npm run build` compiles the React + Tailwind app into `web/dist/` (Vite output).
2. **Copy dist** -- `rm -rf internal/api/dist && cp -r web/dist internal/api/dist` places the built frontend where Go can find it.
3. **go:embed** -- `internal/api/static.go:10` declares `//go:embed all:dist`, which bakes the entire `dist/` directory into the binary at compile time.
4. **go build** -- `go build -o coldstore ./cmd/coldstore` compiles the Go binary with the embedded frontend.

The result is a single `coldstore` binary that serves both the API and the React SPA. The static handler (`internal/api/static.go:13-41`) serves files from the embedded FS and falls back to `index.html` for SPA client-side routing.

## Dev Workflow

Run two terminals:

**Terminal 1 -- Go daemon:**
```bash
make dev
# runs: go run ./cmd/coldstore --config coldstore.toml
# API available at http://127.0.0.1:8090
```

**Terminal 2 -- Vite dev server:**
```bash
make dev-web
# runs: cd web && npm run dev
# Frontend available at http://localhost:5173
```

Vite proxies `/api` requests to `http://127.0.0.1:8090` (`web/vite.config.ts:8-10`), so the React app talks to the real Go API during development. Edit React code and see changes instantly via Vite HMR; edit Go code and restart `make dev`.

## Test Tiers

### Unit Tests (`go test ./...`)

```bash
make test
```

Fast tests with no external dependencies. Run against all packages _except_ those guarded by build tags. Nothing needs to be created first: `internal/api/dist/.gitkeep` is committed, so the `go:embed` directive resolves on a bare checkout -- see [The `dist/` Directory Problem](#the-dist-directory-problem) below.

### E2E Tests (`make test-e2e`)

```bash
make test-e2e
# go test -tags e2e -count=1 -timeout 120s ./tests/e2e/...
```

Guarded by `//go:build e2e`. These create real temp directories, real SQLite databases, and exercise the full daemon pipeline (scan, offload, restore, thumbnails, API). No Docker, no network -- everything runs in-process using `t.TempDir()`.

Key files:
- `tests/e2e/helpers_test.go` -- `newTestEnv()` creates an isolated environment with config, catalog, scanner, offloader, restorer, daemon, and API server.
- `tests/e2e/fixtures_test.go` -- `genFiles()` and `genImageFile()` generate test data with controllable sizes and mtimes.
- `tests/e2e/daemon_scan_test.go` -- scanner tests
- `tests/e2e/daemon_offload_test.go` -- offload logic
- `tests/e2e/daemon_restore_test.go` -- restore logic
- `tests/e2e/api_*.go` -- API endpoint tests using `httptest.Server`

The `-count=1` flag disables test caching so results are always fresh.

### Live E2E Tests (`make test-e2e-live`)

```bash
make test-e2e-live
# go test -tags e2e_live -count=1 -timeout 1800s -v ./tests/e2e/...
```

Guarded by `//go:build e2e_live`, in `tests/e2e/live_*.go`. This is the only tier that
can observe coldstore's central claim, and until this branch it did not exist.

**What it actually does.** `TestMain` (`tests/e2e/live_main_test.go`) owns the whole
lifecycle:

1. `go build -o coldstore ./cmd/coldstore` -- the binary `docker/Dockerfile.test` copies.
   No frontend build: the live tier asserts nothing about the SPA.
2. `docker compose -p coldstore-live -f docker/compose.test.yml up -d --build --wait`.
3. Pairs the two Syncthing devices over their REST APIs: reads each `myID`, registers each
   as a remote of the other at a static compose-network address, and creates the shared
   **sendreceive** folder on both. None of this can be baked into an image -- a device ID
   is derived from a keypair the container generates on first start.
4. Waits for the two devices to connect, for Syncthing to create `.stfolder` in the managed
   folder, and for the coldstore API to answer on the published port.
5. Runs the tests, dumps `docker compose logs` if any failed, and always tears the stack
   down with `down -v`.

Because the lifecycle lives in the suite, `go test -tags e2e_live ./tests/e2e/...` on its
own is a complete run. The Makefile target and the CI job are wrappers that cannot drift
away from it.

**The topology.**

```
syncthing-a  <-- shared folder "livesync" -->  syncthing-b
     |                                              |
device-a-sync volume                        device-b-sync volume
     |
coldstore-test (/data/sync)  ->  cold-storage volume (/data/cold)
```

Device A's Syncthing folder and coldstore's managed folder are **the same volume**, which
is the production arrangement: two writers, one tree. Device B stands in for the owner's
phone -- coldstore cannot see it, cannot reach it, and does not know it exists, so every
assertion about device B is an assertion about something Syncthing did on its own.

**What the live tier genuinely proves** (and nothing else in the repository can):

- A file created on device B reaches the managed folder and is catalogued.
- Archiving that file through the API deletes it from the managed folder **and Syncthing
  propagates the deletion to device B**. This is the product's premise; it had never been
  executed before this suite existed.
- Restoring it through the API puts the bytes back and Syncthing propagates the file back
  to device B, byte-identical by sha256.
- `ffmpeg` is really present and working in the shipped image (the video-thumbnail test).
- The D5 credential gate really rejects an unauthenticated request over a socket.
- A budget lowered through the HTTP API is picked up by the running daemon's next quota
  tick with no restart (D1), a pinned file is never evicted by it (D2), and pinned bytes
  count against the budget and wedge the folder when they exceed it (D3).

**What it does not prove.** See "Coverage limits of the live tier" below.

**Environment knobs**, all with working defaults, all read by `TestMain`:

| Variable | Default | Purpose |
|---|---|---|
| `COLDSTORE_TEST_PORT` | `18090` | Host port for the coldstore API. Not 8090: the deployment host runs a production coldstore there |
| `SYNCTHING_A_PORT` | `18384` | Host port for device A's REST API. Not 8384: a developer machine usually runs Syncthing there |
| `SYNCTHING_B_PORT` | `18385` | Host port for device B's REST API |
| `SYNCTHING_API_KEY` | `coldstore-live-test-key` | `X-API-Key` for both devices, set with `STGUIAPIKEY` |
| `COLDSTORE_LIVE_SKIP_BUILD` | unset | Use the existing `./coldstore` instead of rebuilding |
| `COLDSTORE_LIVE_SKIP_UP` | unset | Assume the stack is already up; only pair and run. Fast iteration |
| `COLDSTORE_LIVE_SKIP_PAIR` | unset | Do not (re)pair the devices. Pairing is an idempotent upsert that rewrites the folder definition on **both** devices, so without this it repairs any Syncthing-level fault you injected before the first test runs -- and the suite proves it cannot go red. This is the knob fault injection needs |
| `COLDSTORE_LIVE_KEEP` | unset | Leave the stack running afterwards. Clean up with `make test-e2e-live-down` |
| `COLDSTORE_LIVE_COMPOSE` | `docker/compose.test.yml` | Alternate compose file, for fault injection |

Fault injection is `COLDSTORE_LIVE_SKIP_UP=1 COLDSTORE_LIVE_SKIP_PAIR=1` against a stack
you have already broken by hand -- pause device B's folder, flip it to `receiveonly`,
stop a container. Both are needed: `SKIP_UP` alone still re-pairs.

The three published ports are checked for availability before anything starts. If a port
is busy the check distinguishes the two cases: a leftover `coldstore-live` project (name
`make test-e2e-live-down`) from something else on the port (name the variable to change).

`TestMain`'s teardown is a `defer`, so it runs on success, on failure and on a panic, and
a signal handler covers Ctrl-C. It does **not** run on a `go test -timeout` expiry: that
panics from the runtime's watchdog goroutine, which terminates the process without
unwinding any other goroutine's defers and cannot be intercepted. That run leaks three
containers, three volumes and three ports -- which is what the port pre-flight above is
for. CI is separately covered by an `if: always()` `docker compose down -v` step.

Neither Syncthing container publishes its transport port (22000): the two devices reach
each other over the compose network, and publishing 22000 collides with the Syncthing the
developer is already running.

### Coverage limits of the live tier

Written down so nobody reads a green run as more than it is.

- **Two containers on one host, one Docker network.** No real LAN, no NAT, no relay, no
  latency, no partition. Global discovery, local discovery, relays and NAT traversal are
  all switched off deliberately, so the suite is hermetic -- and so it says nothing about
  discovery working in the owner's house.
- **Both Syncthing containers run as root** (`PUID=0`), because coldstore also runs as root
  in its container and they share a volume. On the real microserver coldstore runs as the
  Syncthing user, and *that* pairing is asserted only by `coldstore.service` and the
  runbook, not by this suite.
- **The folder is `sendreceive`.** Design 3.7's failure -- a `receiveonly` folder, where
  coldstore's deletions are never propagated and the product is silently void -- is
  described in the design and is **not** tested. There is no Syncthing client in the daemon
  to detect it (see `[syncthing] api_url` / `api_key`, parsed and never read).
- **`*.coldstore-tmp` is checked, not guaranteed.** The temp sibling a restore copies
  through is a real file inside a folder Syncthing is watching. The live test fails only if
  a `.coldstore-tmp` file is left behind permanently, and separately *reports* whether
  Syncthing indexed it at all. On this hardware an 8 MB restore renames faster than
  Syncthing's 1 s watcher delay, so the observation is "never indexed" -- which is
  evidence, not a guarantee. Design 3.8's `.stignore` work is still outstanding.
- **No filesystem faults.** No ENOSPC, no EROFS, no unmounted pool mid-run, no torn write.
  Those live in `internal/daemon/durability_test.go` and `recover_test.go`, in-process.
- **No systemd.** The sandboxing in `coldstore.service` -- above all `ReadWritePaths`, the
  single most likely production failure -- is not exercised here. The container runs the
  bare binary.
- **The SPA is not built into the test image**, so the live tier asserts nothing about the
  frontend. The `frontend` CI job and the Playwright tier own that.

### React unit and component tests (`make test-web`)

```bash
make test-web
# cd web && npm run test   (vitest run)
```

Vitest with jsdom and Testing Library, in `web/src/**/*.test.{ts,tsx}`. No daemon, no
browser: `fetch` is replaced by `stubFetch` (`web/src/test/harness.tsx`), which answers by
URL and method, records every call, and **throws on a request no test set up** -- an
unexpected request is a defect, not a 404 to absorb quietly.

`renderWithToasts` provides the toast context and collects what was reported, so a test
asserts on the sentence the owner is shown rather than on a spy that proves only that
something was called.

What this tier is for: query building, selection semantics, the budget-bar arithmetic, the
pin and restore repaint rules, the 409 confirm dialog's wording, and the error banner. All
of it is deterministic and runs in a couple of seconds.

### Browser tests (`make test-playwright`)

```bash
make test-playwright
# cd web && npx playwright test
```

`web/playwright.config.ts` starts `scripts/playwright-server.sh`, which builds the
frontend into `internal/api/dist/`, seeds a throwaway installation under
`/tmp/coldstore-e2e` with `scripts/seed-demo`, and runs the **real daemon** against it. So
these tests drive the bundle the binary actually ships, over the real API, over a real
SQLite catalog -- not a dev server and not a mock.

`reuseExistingServer` is false and `workers` is 1, both deliberately: the assertions are
written against a freshly seeded catalog, and several tests move real files, so a reused
daemon or a second worker would have them asserting against each other's restores.

The suite covers the flows a unit test cannot: shift-click ranges across the real grid, a
bulk restore that runs as a job and repaints its rows as synced and pinned, the budget
pre-flight dialog, resolving a conflict and watching the row leave the Conflicts tab, a
budget edited in Settings surviving a reload, and a persistent pattern reporting how many
files it really protects.

Browsers are not installed by `npm ci`. Once per machine:

```bash
cd web && npx playwright install --with-deps chromium
```

## CI Pipeline

Defined in `.github/workflows/ci.yml`. Triggers on push to `main` and on pull requests targeting `main`.

### Jobs

**`frontend`** (runs always):
1. Checkout + setup Node 22 with npm cache keyed on `web/package-lock.json`
2. `npm ci`
3. `npm run lint` -- ESLint, including the React 19 `react-hooks` rules
4. `npm run build` -- `tsc -b` typecheck plus the Vite production build
5. `npm run test` -- the vitest unit and component suite

**`playwright`** (runs always), `timeout-minutes: 20`:
1. Checkout + setup Go + setup Node 22
2. Install `ffmpeg` and `poppler-utils` (the daemon generates thumbnails against the
   seeded install, and PDF thumbnails shell out to `pdftocairo`)
3. `npm ci`, then `npx playwright install --with-deps chromium` -- `npm ci` installs the
   runner, never the browsers
4. `npx playwright test`, which starts the daemon itself through
   `scripts/playwright-server.sh`
5. On failure, upload `web/playwright-report` and `web/test-results`

**`fresh-clone-build`** (runs always):
1. Checkout + setup Go + setup Node 22 -- **no npm cache, no `npm ci`**
2. `make build`
3. Assert `./coldstore` and `internal/api/dist/index.html` both exist

This is the deployment runbook's build command run in the state a real owner runs it in:
a checkout that has never been built. `web/node_modules` is gitignored, and `make
frontend` used to go straight to `npm run build`, so a fresh clone died on `sh: tsc:
command not found` before the owner reached the config step. The `frontend` job cannot
catch that because it runs `npm ci` itself. **Do not add an `npm ci` step here** -- the
Makefile's `deps` prerequisite is the fix, and this job is what keeps it fixed.

**`unit`** (runs always):
1. Checkout + setup Go (version from `go.mod`)
2. `go build ./...`
3. `go test ./...`

No `mkdir -p internal/api/dist` is needed in any job: `internal/api/dist/.gitkeep` is committed, so a bare `actions/checkout` already satisfies `//go:embed all:dist`.

**`e2e`** (runs always):
1. Checkout + setup Go
2. Install `ffmpeg` and `poppler-utils` (needed for video and PDF thumbnail tests)
3. `go test -tags e2e -count=1 -timeout 120s ./tests/e2e/...`

`fresh-clone-build` deliberately installs neither. It builds; it does not render.

**`e2e-live`** (runs on PRs and pushes to main), `timeout-minutes: 30`:
1. Checkout + setup Go
2. `make test-e2e-live`
3. `docker compose -p coldstore-live ... down -v || true`, `if: always()`, as belt and braces

There is no separate build step and no separate `docker compose up` step, on purpose. The
job used to build a binary, start a stack, and then run `-tags e2e,e2e_live` -- a tag set
that matched only the in-process `e2e` files, none of which ever opened a socket to the
container. The stack came up, was never touched, and was torn down again, and the job was
green throughout. Moving the lifecycle into `TestMain` means the thing that starts the
containers and the thing that asserts against them are the same program and cannot drift
apart. Do not add `e2e` back to the tag list.

## Docker Setup

### `docker/Dockerfile.test`

```dockerfile
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates curl
WORKDIR /app
COPY coldstore /usr/local/bin/coldstore
COPY tests/fixtures /app/fixtures
COPY docker/coldstore.test.toml /app/coldstore.toml
RUN mkdir -p /data/cold /data/sync
ENTRYPOINT ["coldstore"]
CMD ["--config", "/app/coldstore.toml"]
```

Minimal image: copies the pre-built binary, the test fixture files (`tests/fixtures/minimal.mp4`, `tests/fixtures/minimal.heic`), and the config the container runs with. `ffmpeg` is required for video thumbnail generation, and the live suite has a test that fails if it is missing or broken; `curl` is required by the container health check.

**The config COPY is load-bearing.** The container is started with `--config /app/coldstore.toml`. Without that file the daemon prints `failed to load config` and exits 1 immediately, before binding a port -- and because it exits during startup rather than failing to launch, `docker compose up --wait` would have reported success were it not for the health check.

The build context is the repo root (`context: ..`), which is why the `COPY` paths are `docker/...` and `tests/...` rather than paths relative to `docker/`.

### `docker/compose.test.yml`

Three services and three volumes:

- **coldstore-test** -- runs the daemon with `--config /app/coldstore.toml`. Mounts
  `device-a-sync` at `/data/sync` and `cold-storage` at `/data/cold`, matching
  `docker/coldstore.test.toml`. Publishes `${COLDSTORE_TEST_PORT:-18090}:8090`. Health check
  polls `http://localhost:8090/api/v1/status` **with credentials** -- the daemon refuses a
  non-loopback bind without them (D5), so an unauthenticated probe would only ever see a
  container that exited 1. Waits for `syncthing-a` to be healthy.
- **syncthing-a** -- the microserver's Syncthing. Mounts `device-a-sync` at
  `/var/syncthing/sync`: the **same volume** coldstore manages. Publishes
  `${SYNCTHING_A_PORT:-18384}:8384` so the harness can drive its REST API.
- **syncthing-b** -- the stand-in for the owner's phone. Mounts `device-b-sync`. Publishes
  `${SYNCTHING_B_PORT:-18385}:8384`.

Details worth remembering:

- **Two Syncthing devices, not one.** The stack used to have a single `syncthing` service
  that was never told about any folder, so it proved only that a container can start. With
  one device there is nothing for a deletion to propagate *to*, and coldstore's premise is
  unobservable.
- **`PUID=0` / `PGID=0` on both Syncthing containers.** coldstore runs as root in its
  container and writes into the same volume; a named volume starts out root-owned, so
  Syncthing on its default uid 1000 could not even create `.stfolder`.
- **`STGUIAPIKEY`** gives both devices a fixed API key, so the harness does not have to
  scrape `config.xml` out of a container to talk to them.
- **Neither Syncthing publishes 22000.** The devices reach each other over the compose
  network; publishing the transport port collides with the developer's own Syncthing.
- Syncthing's `/rest/system/ping` requires an API key and answers **403** to an
  unauthenticated probe, so a health check on that path can never pass -- and coldstore-test,
  which depends on `service_healthy`, would never be started at all. `/rest/noauth/health`
  is the endpoint Syncthing exposes for unauthenticated liveness probes.
- Without a health check on coldstore-test, `up --wait` returns as soon as the container is
  *running* -- momentarily true even for a container about to exit 1. The health check is
  what makes a broken image fail the target.

**`docker/Dockerfile.test` deliberately does not create `/data/sync/.stfolder`.** It used
to, because the old stack had no Syncthing folder there and coldstore would otherwise treat
the directory as an unmounted volume (design 3.6) and move nothing. Now `syncthing-a` is
configured with a real folder at that path and creates the marker itself, and `TestMain`
waits for it. Recreating it in the image would paper over a pairing failure and turn 3.6
back into an untested claim.

## Adding a New Test

### Where to put it

| Test type | Location | Build tag |
|---|---|---|
| Unit test for a package | `internal/<pkg>/*_test.go` | none |
| E2E daemon/API test | `tests/e2e/<name>_test.go` | `e2e` |
| Live test against the container and two real Syncthing devices | `tests/e2e/live_<name>_test.go` | `e2e_live` (alone -- never `e2e,e2e_live`) |
| React unit or component test | `web/src/<area>/<name>.test.tsx` | none (vitest) |
| Browser test | `web/e2e/<name>.spec.ts` | none (playwright) |

### Build tag

Every file in `tests/e2e/` must start with a build tag line:

```go
//go:build e2e
```

### Helper functions

Use `newTestEnv()` (`tests/e2e/helpers_test.go:97-148`) to bootstrap a complete test environment. It creates temp dirs, opens a SQLite catalog, and wires up all daemon components. Customize with option functions:

| Option | Purpose | Defined at |
|---|---|---|
| `withBudget(label, bytes)` | Set a folder's budget | `helpers_test.go:35` |
| `withGracePeriod(mins)` | Set the grace period | `helpers_test.go:47` |
| `withPersistent(label, pattern)` | Add a persistent glob pattern | `helpers_test.go:53` |
| `withAuth(user, pass)` | Enable basic auth | `helpers_test.go:65` |
| `withMultiFolders(n)` | Create n folders instead of one | `helpers_test.go:72` |
| `withLiveServer()` | Start an `httptest.Server` for HTTP tests | `helpers_test.go:90` |

Generate test data with:
- `genFiles(t, dir, count, ext, sizeBytes)` (`fixtures_test.go:17`) -- creates files with spread mtimes over 30 days
- `genImageFile(t, path, ext)` (`fixtures_test.go:42`) -- creates a valid 4x4 JPEG or PNG

### Example: adding a scan test

```go
//go:build e2e

package e2e

import "testing"

func TestScanHandlesSymlinks(t *testing.T) {
    env := newTestEnv(t)
    genFiles(t, env.syncDir, 3, ".txt", 500)
    // ... test logic ...
}
```

## The `dist/` Directory Problem

`internal/api/static.go:10` embeds the frontend:

```go
//go:embed all:dist
var distFS embed.FS
```

`go:embed` is evaluated at compile time. If `internal/api/dist/` does not exist, _any_ `go` command that touches the `internal/api` package will fail -- including `go test ./...`, `go vet`, and `go build`.

An empty directory is not enough either: `all:dist` with nothing inside it fails with

```
internal/api/static.go:10:12: pattern all:dist: no matching files found
```

### The fix: one tracked stub file

`internal/api/dist/.gitkeep` is committed. `.gitignore` ignores the *contents* of the
directory but makes an exception for that one file:

```gitignore
internal/api/dist/*
!internal/api/dist/.gitkeep
```

Consequences, all of them intentional:

- A bare `git clone` compiles. `go build ./...`, `go vet ./...` and `go test ./...` all work before Node is installed.
- No CI job and no Makefile target needs `mkdir -p internal/api/dist`. If you find yourself adding one, the stub has gone missing -- restore it instead.
- `make frontend` wipes `dist/` and copies the Vite output in, then `touch`es the stub back. `make clean` does the same. Any target that removes `dist/` must restore `.gitkeep`, or the next fresh build breaks and `git status` reports a deleted tracked file.
- A real Vite build leaves `git status` clean, because `index.html` and `assets/` are ignored.

The stub is a zero-byte `.gitkeep`, not a real `index.html`. `all:` (rather than a bare
`dist`) is what makes `go:embed` include a dot-prefixed file. A binary built without
running `make frontend` therefore embeds an empty-ish FS and serves 404s for the SPA --
that is fine for `go test` and for the e2e-live container, and `make build` always runs
the frontend first.
