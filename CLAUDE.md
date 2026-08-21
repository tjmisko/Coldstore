# Coldstore

A daemon that manages cold storage for Syncthing-synced folders. It monitors folder sizes against configurable budgets and offloads old files to a cold storage path, keeping thumbnails and a browsable catalog available via a web UI.

## Architecture

- **Go daemon** (`cmd/coldstore/`) -- single binary, runs as systemd service
- **React frontend** (`web/`) -- embedded in the binary at build time via `internal/api/dist/`
- **SQLite catalog** -- tracks file metadata, thumbnails, offload state
- **Syncthing integration** -- reads folder config, watches for changes

Key packages:
- `internal/catalog/` -- SQLite-backed file catalog
- `internal/daemon/` -- scanner, offloader, restorer, quota enforcement
- `internal/api/` -- REST API server + embedded frontend
- `internal/config/` -- TOML config parsing
- `internal/thumbnail/` -- thumbnail generation pipeline

## Development

```bash
make dev          # run daemon with local config
make dev-web      # run React dev server (hot reload)
make frontend     # build React app and copy to internal/api/dist/
make build        # build frontend + Go binary
make lint         # eslint the React app (CI runs the same command)
make test         # go test ./...
make install      # install binary to /usr/local/bin
```

`internal/api/dist/.gitkeep` is committed on purpose: `//go:embed all:dist` in
`internal/api/static.go` fails to compile if that directory is missing, so a fresh
clone must already contain it. Never add `mkdir -p internal/api/dist` to a Makefile
target or CI job -- if you need to, the stub was deleted. Any target that removes
`dist/` has to `touch` the stub back. See docs/build-and-test.md.

## Configuration

Copy `coldstore.toml.example` to `coldstore.toml` and edit. See `docs/` for detailed documentation.

## Testing

```bash
go test ./...                                          # unit
go test -tags e2e -count=1 ./tests/e2e/...             # in-process e2e
make test-e2e-live                                     # docker: the daemon + TWO real syncthing devices
cd web && npm run lint                                 # eslint
```

The live tier (`tests/e2e/live_*.go`, tag `e2e_live` **alone**) drives the containerised
daemon over its published port and two paired Syncthing devices over their REST APIs. It
is the only place the product's central claim is executed: that archiving a file makes
Syncthing delete it from the owner's other devices, and restoring brings it back. Its
`TestMain` owns the containers -- build, up, pair, run, log on failure, `down -v` -- so
`go test -tags e2e_live ./tests/e2e/...` is a complete run on its own. Never add `e2e` to
that tag list: that only re-runs the in-process suite, which is how the target spent its
whole life starting a stack it never talked to. Coverage limits are written down in
docs/build-and-test.md.

CI (`.github/workflows/ci.yml`) runs five jobs: `frontend` (eslint + tsc + vite build),
`fresh-clone-build`, `unit`, `e2e`, and `e2e-live`.

`fresh-clone-build` runs `make build` with **no preceding `npm ci`**, because that is
the state the deployment runbook's build command actually runs in (`web/node_modules` is
gitignored). Do not add an `npm ci` step to it -- that is the bug it exists to catch.
