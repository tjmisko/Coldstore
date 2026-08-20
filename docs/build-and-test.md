# Build Pipeline and Testing Guide

## Makefile Targets

| Target | What it does | When to use it |
|---|---|---|
| `frontend` | Runs `npm run build` in `web/`, then copies `web/dist` to `internal/api/dist/` | After changing React code, before building the binary |
| `build` | Runs `frontend`, then `go build -o coldstore ./cmd/coldstore` | Produce a release binary with embedded frontend |
| `dev` | `go run ./cmd/coldstore --config coldstore.toml` | Run the Go daemon locally during development |
| `dev-web` | `cd web && npm run dev` (Vite dev server) | Run the React frontend with hot reload |
| `lint` | `cd web && npm run lint` (ESLint) | Check the React code before pushing; CI runs the same command |
| `test` | `go test ./...` | Run unit tests (no build tags) |
| `test-e2e` | `go test -tags e2e -count=1 -timeout 120s ./tests/e2e/...` | Run e2e tests against real SQLite + temp dirs |
| `test-e2e-live` | Build binary, start Docker Compose, run `-tags e2e,e2e_live` tests, tear down | Test with real Syncthing in Docker |
| `test-playwright` | `go test -tags playwright ./tests/e2e/...` | Run browser-level UI tests (stubs for now) |
| `clean` | Remove the `coldstore` binary and the built frontend, then restore the tracked `internal/api/dist/.gitkeep` stub | Reset build artifacts |
| `install` | Run `build`, then install binary to `/usr/local/bin/coldstore` | Install on the target machine |

Source: `Makefile`

## Build Pipeline Walkthrough

`make build` produces a single self-contained binary in four steps:

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
```

Requires the `e2e,e2e_live` build tags. This target:

1. Builds the full binary (`make build` dependency, `Makefile:27`)
2. Starts Docker Compose (`docker/compose.test.yml`) with a real Syncthing instance and the coldstore binary
3. Runs tests with a 300s timeout
4. Tears down containers regardless of test outcome

The Docker Compose stack (`docker/compose.test.yml`) defines:
- **coldstore-test** -- Debian slim container with the built binary, `ffmpeg`, `curl`, and `docker/coldstore.test.toml` baked in at `/app/coldstore.toml`. Its own health check polls `/api/v1/status`, so `up --wait` does not return until the daemon is actually answering.
- **syncthing** -- Official Syncthing image with a health check that polls `/rest/noauth/health`. It publishes no host ports: only coldstore-test talks to it, over the compose network.
- Shared volumes (`shared-sync`, `cold-storage`) connect the two services

The coldstore host port defaults to 8090 and is overridable, which matters on the
deployment host itself (where a production coldstore is already bound to 8090):

```bash
COLDSTORE_TEST_PORT=18090 make test-e2e-live
```

The target propagates the `go test` exit status: containers are always torn down, but a
test failure fails the target.

### Playwright Stubs

```bash
make test-playwright
# go test -tags playwright ./tests/e2e/...
```

Guarded by `//go:build playwright`. Currently all four tests skip immediately (`tests/e2e/playwright_stub_test.go:1-22`). Placeholders exist for:
- Gallery view
- File browser
- Restore flow
- Config panel

## CI Pipeline

Defined in `.github/workflows/ci.yml`. Triggers on push to `main` and on pull requests targeting `main`.

### Jobs

**`frontend`** (runs always):
1. Checkout + setup Node 22 with npm cache keyed on `web/package-lock.json`
2. `npm ci`
3. `npm run lint` -- ESLint, including the React 19 `react-hooks` rules
4. `npm run build` -- `tsc -b` typecheck plus the Vite production build

**`unit`** (runs always):
1. Checkout + setup Go (version from `go.mod`)
2. `go build ./...`
3. `go test ./...`

No `mkdir -p internal/api/dist` is needed in any job: `internal/api/dist/.gitkeep` is committed, so a bare `actions/checkout` already satisfies `//go:embed all:dist`.

**`e2e`** (runs always):
1. Checkout + setup Go
2. Install `ffmpeg` (needed for video thumbnail tests)
3. `go test -tags e2e -count=1 -timeout 120s ./tests/e2e/...`

**`e2e-live`** (runs on PRs and pushes to main, per `ci.yml:32`):
1. Checkout + setup Go
2. `go build -o coldstore ./cmd/coldstore` -- a stub-frontend binary, since live tests do not exercise the SPA. `docker/Dockerfile.test` COPYs this binary from the repo root.
3. `docker compose -f docker/compose.test.yml up -d --build --wait`
4. Run live tests: `go test -tags e2e,e2e_live -count=1 -timeout 300s`
5. On failure, dump `docker compose logs`
6. Tear down (runs even on failure, per `if: always()`)

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

Minimal image: copies the pre-built binary, the test fixture files (e.g., `tests/fixtures/minimal.mp4`, `tests/fixtures/minimal.heic`), and the config the container runs with. `ffmpeg` is required for video thumbnail generation; `curl` is required by the container health check.

**The config COPY is load-bearing.** The container is started with `--config /app/coldstore.toml`. Without that file the daemon prints `failed to load config` and exits 1 immediately, before binding a port -- and because it exits during startup rather than failing to launch, `docker compose up --wait` would have reported success were it not for the health check.

The build context is the repo root (`context: ..`), which is why the `COPY` paths are `docker/...` and `tests/...` rather than paths relative to `docker/`.

### `docker/compose.test.yml`

Two services:
- **coldstore-test**: Runs the daemon with `--config /app/coldstore.toml`. Mounts `shared-sync` at `/data/sync` and `cold-storage` at `/data/cold`, matching `docker/coldstore.test.toml`. Publishes `${COLDSTORE_TEST_PORT:-8090}:8090`. Health check polls `http://localhost:8090/api/v1/status`. Waits for Syncthing to be healthy before starting (`depends_on` with `condition: service_healthy`).
- **syncthing**: Official image. Health check polls `http://localhost:8384/rest/noauth/health` every 5s. Shares the `shared-sync` volume with coldstore-test at `/var/syncthing/sync`.

Two health check details worth remembering:

- Syncthing's `/rest/system/ping` requires an API key and answers **403** to an unauthenticated probe. A health check on that path can never pass, and since coldstore-test depends on `service_healthy`, it would never be started at all. `/rest/noauth/health` is the endpoint Syncthing exposes for unauthenticated liveness probes.
- Without a health check on coldstore-test, `up --wait` returns as soon as the container is *running* -- which is momentarily true even for a container that is about to exit 1. The health check is what makes a broken image fail the target.

## Adding a New Test

### Where to put it

| Test type | Location | Build tag |
|---|---|---|
| Unit test for a package | `internal/<pkg>/*_test.go` | none |
| E2E daemon/API test | `tests/e2e/<name>_test.go` | `e2e` |
| E2E with real Syncthing | `tests/e2e/<name>_test.go` | `e2e,e2e_live` |
| Browser test | `tests/e2e/<name>_test.go` | `playwright` |

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
