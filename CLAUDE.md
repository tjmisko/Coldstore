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
make test-e2e-live                                     # docker: coldstore + real syncthing
cd web && npm run lint                                 # eslint
```

CI (`.github/workflows/ci.yml`) runs four jobs: `frontend` (eslint + tsc + vite build),
`unit`, `e2e`, and `e2e-live`.
