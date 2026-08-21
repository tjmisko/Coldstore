.PHONY: build binary deps dev dev-web lint test test-e2e test-e2e-live test-e2e-live-down test-playwright clean install install-binary frontend

BINARY := coldstore
BUILD_DIR := ./cmd/coldstore

# internal/api/dist/.gitkeep is tracked in git so that //go:embed all:dist resolves on a
# clean checkout. Any target that wipes dist/ must put the stub back, or the next
# `go build` on a fresh tree fails and `git status` reports a deleted tracked file.
# web/node_modules is gitignored, so on a fresh clone `npm run build` fails with
# `tsc: command not found` -- which is what the deployment runbook used to walk
# an owner straight into. Installing here rather than in the runbook keeps the
# one documented build command working on a tree that has never been built.
# It is skipped when node_modules already exists, so an incremental build is
# still just vite.
deps:
	cd web && { [ -d node_modules ] || npm ci; }

frontend: deps
	cd web && npm run build
	rm -rf internal/api/dist
	cp -r web/dist internal/api/dist
	touch internal/api/dist/.gitkeep

build: frontend
	go build -o $(BINARY) $(BUILD_DIR)

# The Go binary alone, with whatever is currently in internal/api/dist. This is
# what docker/Dockerfile.test copies; the live suite asserts nothing about the
# SPA, and requiring a working npm toolchain to run an integration test is a
# reason not to run it.
binary:
	go build -o $(BINARY) $(BUILD_DIR)

dev:
	go run $(BUILD_DIR) --config coldstore.toml

dev-web:
	cd web && npm run dev

lint:
	cd web && npm run lint

test:
	go test ./...

test-e2e:
	go test -tags e2e -count=1 -timeout 120s ./tests/e2e/...

# The live tier. `-tags e2e_live` builds ONLY the tests/e2e/live_*.go files: they
# talk to the containerised daemon over its published port and to two real
# Syncthing devices over their REST APIs.
#
# This target used to run `-tags e2e,e2e_live`, which rebuilt and re-ran the
# entire in-process suite -- the same tests `make test-e2e` had just run, none of
# which ever opened a socket to the container -- and no file in the repository
# carried the e2e_live tag at all. The stack came up, was never touched, and was
# torn down again. Do not put `e2e` back in that tag list.
#
# Bringing the stack up and tearing it down is TestMain's job (see
# tests/e2e/live_main_test.go), not this target's, so that a plain
# `go test -tags e2e_live ./tests/e2e/...` is a complete run and cannot drift
# away from what the Makefile and CI do. -v is deliberate: several live tests
# report an observation that is not an assertion.
test-e2e-live:
	go test -tags e2e_live -count=1 -timeout 1800s -v ./tests/e2e/...

# Escape hatch: tear the live stack down by hand after COLDSTORE_LIVE_KEEP=1 or
# an interrupted run. The project name matches composeProject in the harness.
test-e2e-live-down:
	docker compose -p coldstore-live -f docker/compose.test.yml down -v --remove-orphans

test-playwright:
	go test -tags playwright ./tests/e2e/...

clean:
	rm -f $(BINARY)
	rm -rf internal/api/dist
	mkdir -p internal/api/dist
	touch internal/api/dist/.gitkeep

# Install an ALREADY-BUILT binary. `make build && sudo make install-binary` is
# what the runbook says, because `sudo make install` runs the whole build as
# root and leaves root-owned web/dist/, internal/api/dist/ and ./coldstore in
# the checkout, which breaks the next non-sudo `make build`.
install-binary:
	install -m 755 $(BINARY) /usr/local/bin/$(BINARY)

install: build install-binary
