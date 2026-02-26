.PHONY: build dev dev-web test clean install frontend

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
	go test ./...

clean:
	rm -f $(BINARY)
	rm -rf internal/api/dist

install: build
	install -m 755 $(BINARY) /usr/local/bin/$(BINARY)
