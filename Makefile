.PHONY: build test clean install run-fetch run-list help

# Binary name
BINARY=herald

# Build directories
BUILD_DIR=.

help:
	@echo "FeedReader - Makefile commands:"
	@echo "  make build      - Build the application"
	@echo "  make test       - Run tests"
	@echo "  make clean      - Remove build artifacts"
	@echo "  make install    - Install binary to /usr/local/bin"
	@echo "  make run-fetch  - Run feed fetch"
	@echo "  make run-list   - List unread articles"

build:
	@echo "Building $(BINARY)..."
	@go build -o $(BINARY) ./cmd/herald

test:
	@echo "Running tests..."
	@go test -v ./...

clean:
	@echo "Cleaning..."
	@rm -f $(BINARY)
	@rm -f *.db *.db-shm *.db-wal
	@rm -f internal/storage/test.db
	@echo "Clean complete"

install: build
	@echo "Installing $(BINARY) to /usr/local/bin..."
	@sudo cp $(BINARY) /usr/local/bin/
	@echo "Installation complete"

run-fetch: build
	@./$(BINARY) fetch

run-list: build
	@./$(BINARY) list --limit 10

init-config: build
	@./$(BINARY) init-config
