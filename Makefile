.PHONY: build dev dev-web test test-e2e test-e2e-live test-playwright clean install frontend

BINARY := coldstore
BUILD_DIR := ./cmd/coldstore

frontend:
	cd web && npm run build
	rm -rf internal/api/dist
	cp -r web/dist internal/api/dist

build: frontend
	go build -o $(BINARY) $(BUILD_DIR)

dev:
	go run $(BUILD_DIR) --config coldstore.toml

dev-web:
	cd web && npm run dev

test:
	@mkdir -p internal/api/dist
	go test ./...

test-e2e:
	go test -tags e2e -count=1 -timeout 120s ./tests/e2e/...

test-e2e-live: build
	docker compose -f docker/compose.test.yml up -d --build --wait
	go test -tags e2e,e2e_live -count=1 -timeout 300s ./tests/e2e/... || true
	docker compose -f docker/compose.test.yml down -v

test-playwright:
	go test -tags playwright ./tests/e2e/...

clean:
	rm -f $(BINARY)
	rm -rf internal/api/dist

install: build
	install -m 755 $(BINARY) /usr/local/bin/$(BINARY)
