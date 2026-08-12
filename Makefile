# CGO off keeps builds static and cross-compilation trivial. Note for Phase 4:
# this commits us to a pure-Go SQLite driver (modernc.org/sqlite). The common
# alternative, mattn/go-sqlite3, requires cgo and will not build under this.
export CGO_ENABLED := 0

.PHONY: build test test-v coverage fmt vet lint check clean help

# Compile every package. Once cmd/htxdev exists this becomes:
#   go build -o bin/htxdev ./cmd/htxdev
build:
	go build ./...

# Run tests
test:
	go test ./...

# Run tests with per-test output
test-v:
	go test -v ./...

# Test coverage report
coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

# Format
fmt:
	gofmt -w .

# Vet
vet:
	go vet ./...

# Lint. golangci-lint is provided by Compendium, not the base PATH, so this
# needs an activated shell first:
#   eval "$(compendium activate)"
lint:
	golangci-lint run

# What CI should run before a commit is worth pushing
check: fmt vet test

# Clean build artifacts. Never touches htxdev.db, which is a committed,
# permanent record rather than a build artifact.
clean:
	rm -rf bin/
	rm -f coverage.out
	go clean

# Display help
help:
	@echo "Available targets:"
	@echo "  make build    - Compile every package"
	@echo "  make test     - Run tests"
	@echo "  make test-v   - Run tests with per-test output"
	@echo "  make coverage - Test coverage report"
	@echo "  make fmt      - Format the Go code"
	@echo "  make vet      - Vet the Go code"
	@echo "  make lint     - Lint the Go code (needs golangci-lint)"
	@echo "  make check    - fmt + vet + test"
	@echo "  make clean    - Clean build artifacts"
	@echo "  make help     - Display this help message"
