#!/bin/sh
# The server Playwright drives: the real daemon, serving the real embedded
# frontend, over a freshly seeded demo installation.
#
# Every step matters. `make frontend` is what puts the built React app into
# internal/api/dist, so the suite exercises the bundle the binary ships rather
# than a Vite dev server. seed-demo rebuilds the catalog from scratch, so a run
# never inherits the previous run's pins, budgets or resolved conflicts.
set -eu

cd "$(dirname "$0")/.."

ROOT=${COLDSTORE_E2E_ROOT:-/tmp/coldstore-e2e}
ADDR=${COLDSTORE_E2E_ADDR:-127.0.0.1:8091}

make frontend
go run ./scripts/seed-demo -root "$ROOT" -addr "$ADDR"
exec go run ./cmd/coldstore --config "$ROOT/coldstore.toml"
