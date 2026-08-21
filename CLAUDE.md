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
- `internal/config/` -- TOML config parsing
- `internal/api/` -- REST API server + embedded frontend
- `internal/thumbnail/` -- thumbnail generation pipeline
- `internal/filetype/` -- magic-number sniffing; the only way a file with no
  extension is ever classified
- `internal/epub/` -- EPUB container reader, shared by the cover thumbnailer and
  the preview API

External binaries: **ffmpeg** (HEIF/HEIC and video) and **poppler-utils**
(`pdftocairo`, for PDF thumbnails). Both are runtime-only: the build needs
neither, and `fresh-clone-build` deliberately installs neither. `pdftoppm` cannot
write to stdout -- given `-` it creates a file called `-.jpg` -- so the PDF path
uses `pdftocairo` and must keep doing so.

## Development

```bash
make dev          # run daemon with local config
make dev-web      # run React dev server (hot reload)
make frontend     # build React app and copy to internal/api/dist/
make build        # build frontend + Go binary
make lint         # eslint the React app (CI runs the same command)
make test         # go test ./...
make test-web     # vitest: React unit and component tests (jsdom, stubbed fetch)
make test-playwright  # chromium against the real daemon over a seeded demo install
make demo         # seed a demo installation and run the daemon on it, :8090
make install      # install binary to /usr/local/bin
```

`internal/api/dist/.gitkeep` is committed on purpose: `//go:embed all:dist` in
`internal/api/static.go` fails to compile if that directory is missing, so a fresh
clone must already contain it. Never add `mkdir -p internal/api/dist` to a Makefile
target or CI job -- if you need to, the stub was deleted. Any target that removes
`dist/` has to `touch` the stub back. See docs/build-and-test.md.

## Configuration

Copy `coldstore.toml.example` to `coldstore.toml` and edit. See `docs/` for detailed documentation.

The current product designs are:

- `docs/interface-refresh.md` — interface, preview and operational-status acceptance;
- `docs/keymap-design.md` — keyboard grammar and interaction contract; and
- `docs/tagging-design.md` — verified metadata findings plus the tagging, Live Photo and
  rename plan.

Schema numbers in design documents are assigned in landing order and are never reused after
another branch ships them. Document preview owns migration 6; tagging and rename are planned
as migrations 7 and 8.

## Testing

```bash
go test ./...                                          # unit
go test -tags e2e -count=1 ./tests/e2e/...             # in-process e2e
make test-e2e-live                                     # docker: the daemon + TWO real syncthing devices
make test-web                                          # vitest + Testing Library, jsdom
make test-playwright                                   # chromium against the real daemon
cd web && npm run lint                                 # eslint
```

The frontend has two tiers of its own. `make test-web` is jsdom with `fetch` stubbed by
`web/src/test/harness.tsx`, which throws on a request no test set up. `make
test-playwright` starts `scripts/playwright-server.sh`, which builds the bundle into
`internal/api/dist/`, seeds a throwaway installation with `scripts/seed-demo` and runs the
real binary against it -- so the browser tier drives the embedded UI, not a dev server.
Browsers are not installed by `npm ci`: `cd web && npx playwright install --with-deps
chromium`, once per machine.

`scripts/seed-demo` is the fixture both the demo and the browser tier depend on. It builds
four folders covering every state the control plane renders: pins of both `pin_source`
values, a wedged folder, a disabled one, one no longer declared in `coldstore.toml`, a
conflicted file and thumbnail failures. Without it half the UI is unreachable, which is
how it went unexercised for as long as it did. It refuses to delete a `-root` it did not
create (marker file `.coldstore-demo`).

Its documents are REAL files -- a hand-assembled PDF with a correct xref table, a
conforming EPUB whose OPF sits in `OEBPS/`, one deliberately corrupt PDF so the
failure glyph stays reachable, and one file with no extension at all. They used to
be counting-byte filler with a `.pdf` name on it, which was fine until the
generator learned PDF and then made the Playwright suite go red. It also sniffs
each file the way the scanner does: the Documents folder is seeded DISABLED, so no
scan ever reaches it, and without that its extension-less file would stay
unclassified and unpreviewable.

The live tier (`tests/e2e/live_*.go`, tag `e2e_live` **alone**) drives the containerised
daemon over its published port and two paired Syncthing devices over their REST APIs. It
is the only place the product's central claim is executed: that archiving a file makes
Syncthing delete it from the owner's other devices, and restoring brings it back. Its
`TestMain` owns the containers -- build, up, pair, run, log on failure, `down -v` -- so
`go test -tags e2e_live ./tests/e2e/...` is a complete run on its own. Never add `e2e` to
that tag list: that only re-runs the in-process suite, which is how the target spent its
whole life starting a stack it never talked to. Coverage limits are written down in
docs/build-and-test.md.

CI (`.github/workflows/ci.yml`) runs six jobs: `frontend` (eslint + tsc + vite build +
vitest), `fresh-clone-build`, `unit`, `e2e`, `playwright`, and `e2e-live`.

`fresh-clone-build` runs `make build` with **no preceding `npm ci`**, because that is
the state the deployment runbook's build command actually runs in (`web/node_modules` is
gitignored). Do not add an `npm ci` step to it -- that is the bug it exists to catch.
