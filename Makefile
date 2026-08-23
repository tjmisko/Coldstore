.PHONY: build binary demo deps node-modules-boundary dev dev-web lint test test-web test-e2e test-e2e-live test-e2e-live-down test-playwright clean install install-binary frontend icons redeploy-simple

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
deps: web/node_modules/.package-lock.json
	@$(MAKE) --no-print-directory node-modules-boundary

# npm writes .package-lock.json inside node_modules to describe the tree it
# actually installed, so it is the honest stamp for "these deps are current".
#
# The previous guard was `[ -d node_modules ] || npm ci`, which only ever fired
# on a tree that had never been built. A checkout that HAD been built and then
# pulled new dependencies kept its old node_modules forever: `make test-web`
# died with `vitest: command not found` months after vitest was added, and
# `make frontend` was worse -- it succeeded, and shipped a bundle built against
# a dependency tree nobody could name. CI never sees this, because CI always
# starts from a clean clone; only a long-lived working copy hits it, which is
# every machine that actually builds a release.
web/node_modules/.package-lock.json: web/package-lock.json
	cd web && npm ci
	@touch $@

# `go build ./...` walks into web/node_modules, and npm packages do ship Go
# source -- eslint pulls in `flatted`, which carries a golang/ directory, so a
# plain `go test ./...` after an npm install lists a package from node_modules.
# Today it merely compiles; the day a transitive dependency ships Go that does
# not, the Go build breaks for a reason nobody would think to look for. A
# nested go.mod is a module boundary, and the parent's ./... stops at it.
node-modules-boundary:
	@[ -d web/node_modules ] && printf 'module coldstore/web-node-modules-boundary\n\ngo 1.24\n' \
		> web/node_modules/go.mod || true

frontend: deps
	cd web && npm run build
	mkdir -p internal/api/dist
	find internal/api/dist -mindepth 1 -delete
	cp -r web/dist/. internal/api/dist/
	touch internal/api/dist/.gitkeep

build: frontend
	go build -o $(BINARY) $(BUILD_DIR)

# Regenerates the raster app icons from the SVG sources in web/icons/. The PNGs
# are committed, so this is a manual step after editing the mark -- ImageMagick
# is NOT a build dependency and no other target depends on this one.
icons:
	magick -background none web/icons/app-icon.svg -resize 180x180 png:web/public/apple-touch-icon.png
	magick -background none web/icons/app-icon.svg -resize 192x192 png:web/public/icon-192.png
	magick -background none web/icons/app-icon.svg -resize 512x512 png:web/public/icon-512.png
	magick -background none web/icons/app-icon-maskable.svg -resize 512x512 png:web/public/icon-maskable-512.png

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

# The React unit and component tier: vitest + Testing Library, jsdom, no
# daemon and no browser. Fast enough to run on every save.
test-web: deps
	cd web && npm run test

# The browser tier. scripts/playwright-server.sh builds the frontend, seeds a
# throwaway demo installation and starts the REAL daemon, so these tests drive
# the bundle the binary ships rather than a dev server.
#
# This target used to run `go test -tags playwright`, whose only file was four
# t.Skip stubs -- a green target that asserted nothing.
test-playwright: deps
	cd web && npx playwright test

# Seed and run a demo installation: four folders covering every state the
# control plane can render, on http://127.0.0.1:8090.
demo: binary
	go run ./scripts/seed-demo
	./$(BINARY) --config /tmp/coldstore-demo/coldstore.toml

clean:
	rm -f $(BINARY)
	find internal/api/dist -mindepth 1 -delete
	mkdir -p internal/api/dist
	touch internal/api/dist/.gitkeep

# Install an ALREADY-BUILT binary. `make build && sudo make install-binary` is
# what the runbook says, because `sudo make install` runs the whole build as
# root and leaves root-owned web/dist/, internal/api/dist/ and ./coldstore in
# the checkout, which breaks the next non-sudo `make build`.
install-binary:
	install -m 755 $(BINARY) /usr/local/bin/$(BINARY)

install: build install-binary

# The routine, schema-preserving production path. The script pulls main, builds
# before downtime, verifies the release and archive mount, creates rollback
# backups, and restores the previous binary automatically if startup fails.
redeploy-simple:
	./scripts/redeploy-simple
