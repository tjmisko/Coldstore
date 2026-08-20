.PHONY: build dev dev-web lint test test-e2e test-e2e-live test-playwright clean install frontend

BINARY := coldstore
BUILD_DIR := ./cmd/coldstore

# internal/api/dist/.gitkeep is tracked in git so that //go:embed all:dist resolves on a
# clean checkout. Any target that wipes dist/ must put the stub back, or the next
# `go build` on a fresh tree fails and `git status` reports a deleted tracked file.
frontend:
	cd web && npm run build
	rm -rf internal/api/dist
	cp -r web/dist internal/api/dist
	touch internal/api/dist/.gitkeep

build: frontend
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

test-e2e-live: build
	docker compose -f docker/compose.test.yml up -d --build --wait
	go test -tags e2e,e2e_live -count=1 -timeout 300s ./tests/e2e/...; \
	  status=$$?; \
	  docker compose -f docker/compose.test.yml down -v; \
	  exit $$status

test-playwright:
	go test -tags playwright ./tests/e2e/...

clean:
	rm -f $(BINARY)
	rm -rf internal/api/dist
	mkdir -p internal/api/dist
	touch internal/api/dist/.gitkeep

install: build
	install -m 755 $(BINARY) /usr/local/bin/$(BINARY)
