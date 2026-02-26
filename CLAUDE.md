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
make test         # go test ./...
make install      # install binary to /usr/local/bin
```

## Configuration

Copy `coldstore.toml.example` to `coldstore.toml` and edit. See `docs/` for detailed documentation.

## Testing

```bash
go test ./...
```
