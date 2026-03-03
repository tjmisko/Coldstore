# Build Pipeline and Testing Guide

## Makefile Targets

| Target | What it does | When to use it |
|---|---|---|
| `frontend` | Runs `npm run build` in `web/`, then copies `web/dist` to `internal/api/dist/` | After changing React code, before building the binary |
| `build` | Runs `frontend`, then `go build -o coldstore ./cmd/coldstore` | Produce a release binary with embedded frontend |
| `dev` | `go run ./cmd/coldstore --config coldstore.toml` | Run the Go daemon locally during development |
| `dev-web` | `cd web && npm run dev` (Vite dev server) | Run the React frontend with hot reload |
| `test` | `mkdir -p internal/api/dist && go test ./...` | Run unit tests (no build tags) |
| `test-e2e` | `go test -tags e2e -count=1 -timeout 120s ./tests/e2e/...` | Run e2e tests against real SQLite + temp dirs |
| `test-e2e-live` | Build binary, start Docker Compose, run `-tags e2e,e2e_live` tests, tear down | Test with real Syncthing in Docker |
| `test-playwright` | `go test -tags playwright ./tests/e2e/...` | Run browser-level UI tests (stubs for now) |
| `clean` | Remove `coldstore` binary and `internal/api/dist/` | Reset build artifacts |
| `install` | Run `build`, then install binary to `/usr/local/bin/coldstore` | Install on the target machine |

Source: `Makefile:1-41`

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

Fast tests with no external dependencies. Run against all packages _except_ those guarded by build tags. The `mkdir -p internal/api/dist` in the Makefile (`Makefile:21`) ensures the `go:embed` directive doesn't fail -- see [The `dist/` Directory Problem](#the-dist-directory-problem) below.

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

The Docker Compose stack (`docker/compose.test.yml:1-31`) defines:
- **coldstore-test** -- Debian slim container with the built binary and `ffmpeg`, reading from shared volumes
- **syncthing** -- Official Syncthing image with a health check that polls `/rest/system/ping`
- Shared volumes (`shared-sync`, `cold-storage`) connect the two services

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

**`unit`** (runs always):
1. Checkout + setup Go (version from `go.mod`)
2. `go test ./...`

No `mkdir -p internal/api/dist` needed in CI because the checkout includes `internal/api/dist/` (it's committed with a stub `index.html`).

**`e2e`** (runs always):
1. Checkout + setup Go
2. Install `ffmpeg` (needed for video thumbnail tests)
3. `go test -tags e2e -count=1 -timeout 120s ./tests/e2e/...`

**`e2e-live`** (runs on PRs and pushes to main, per `ci.yml:32`):
1. Checkout + setup Go
2. Build a stub binary: `mkdir -p internal/api/dist && touch internal/api/dist/index.html && go build` (`ci.yml:40-42`). This skips the full `npm build` since live tests don't exercise the frontend.
3. `docker compose -f docker/compose.test.yml up -d --build --wait`
4. Run live tests: `go test -tags e2e,e2e_live -count=1 -timeout 300s`
5. Tear down (runs even on failure, per `if: always()` at `ci.yml:48`)

## Docker Setup

### `docker/Dockerfile.test`

```dockerfile
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates
WORKDIR /app
COPY coldstore /usr/local/bin/coldstore
COPY tests/fixtures /app/fixtures
ENTRYPOINT ["coldstore"]
```

Minimal image: copies the pre-built binary and test fixture files (e.g., `tests/fixtures/minimal.mp4`, `tests/fixtures/minimal.heic`). The `ffmpeg` dependency is required for video thumbnail generation.

### `docker/compose.test.yml`

Two services:
- **coldstore-test**: Runs the daemon with `--config /app/coldstore.toml`. Mounts `shared-sync` and `cold-storage` volumes. Exposes port 8090. Waits for Syncthing to be healthy before starting (`depends_on` with `condition: service_healthy`).
- **syncthing**: Official image. Health check polls `http://localhost:8384/rest/system/ping` every 5s with 10 retries. Shares the `shared-sync` volume with coldstore-test.

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

This matters in three situations:

1. **After `make clean`** -- which deletes `internal/api/dist/`. Running `go test ./...` immediately after will fail.
2. **Fresh clone without `npm install`** -- if `dist/` is not committed, tests break before you even set up Node.
3. **CI builds** -- the `e2e-live` job needs to compile the binary but doesn't need a real frontend, so it creates a stub (`ci.yml:40-41`).

`make test` handles this with `mkdir -p internal/api/dist` (`Makefile:21`). If you're running `go test` directly, create the directory first:

```bash
mkdir -p internal/api/dist
go test ./...
```

The repo currently commits a built `dist/` with a real `index.html` and `assets/` directory so that `go test` works out of the box after clone. If you run `make clean`, remember to run `make frontend` or `mkdir -p internal/api/dist` before running tests again.
