.PHONY: build dev test clean install

BINARY := coldstore
BUILD_DIR := ./cmd/coldstore

build:
	go build -o $(BINARY) $(BUILD_DIR)

dev:
	go run $(BUILD_DIR) --config coldstore.toml

test:
	go test ./...

clean:
	rm -f $(BINARY)

install: build
	install -m 755 $(BINARY) /usr/local/bin/$(BINARY)
