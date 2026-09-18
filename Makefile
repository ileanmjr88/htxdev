# CGO off keeps builds static and cross-compilation trivial. Note for Phase 4:
# this commits us to a pure-Go SQLite driver (modernc.org/sqlite). The common
# alternative, mattn/go-sqlite3, requires cgo and will not build under this.
export CGO_ENABLED := 0

.PHONY: build run sync-dry db events events-preview site site-dev site-install test test-v test-race coverage fmt vet lint check clean help

# Compile every package, then link the binary. Both, not just the binary:
# `go build ./...` is what catches a package that no longer compiles but that
# cmd/htxdev does not import.
build:
	go build ./...
	go build -o bin/htxdev ./cmd/htxdev

# Fetch every enabled source, write to htxdev.db, and print what came back.
# Reads data/sources.yaml relative to the repo root, so run it from here.
run:
	go run ./cmd/htxdev sync

# The same, without touching the database. htxdev.db is a committed artifact,
# so looking before writing is a question worth being able to ask.
sync-dry:
	go run ./cmd/htxdev sync -n -v

# Open the database. Read-only, so an exploratory session cannot dirty a file
# that is about to be committed.
db:
	sqlite3 -readonly -header -column htxdev.db

# Write data/events.json from the database. Published events only, which today
# means none: see `make events-preview`.
#
# Not called `export`, which is a Make directive.
events:
	go run ./cmd/htxdev export

# The same, including events from groups nobody has verified. For looking at
# locally. The site marks the page as a preview and adds a noindex, because a
# preview that looks like the real thing is how unverified data ends up
# somewhere public.
events-preview:
	go run ./cmd/htxdev export -preview

# The site. Every npm target runs from THIS directory, never from site/,
# because `compendium activate` reads compendium.toml from the working
# directory: run npm inside site/ and activation silently fails, PATH keeps
# whatever node is already on it, and the pinned toolchain is bypassed with no
# error anybody would notice. That is the exact failure Compendium exists to
# prevent, and it takes one cd to cause.
site-install:
	npm --prefix site install

site: events-preview
	npm --prefix site run build

site-dev:
	npm --prefix site run dev

# Run tests
test:
	go test ./...

# Run tests with per-test output
test-v:
	go test -v ./...

# Run tests under the race detector. Separate from `test` because it is slower,
# but not optional: internal/fetch runs a worker pool, and a data race there is
# exactly the kind of bug that passes a plain `go test` every time.
test-race:
	go test -race ./...

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

# What CI should run before a commit is worth pushing. Includes the race
# detector, because the concurrency in internal/fetch is the part of this repo
# most likely to break silently.
check: fmt vet test-race

# Clean build artifacts. Never touches htxdev.db, which is a committed,
# permanent record rather than a build artifact.
clean:
	rm -rf bin/
	rm -f coverage.out
	go clean

# Display help
help:
	@echo "Available targets:"
	@echo "  make build     - Compile every package and link bin/htxdev"
	@echo "  make run       - Fetch every enabled source into htxdev.db"
	@echo "  make sync-dry  - Fetch and list everything, writing nothing"
	@echo "  make db        - Open htxdev.db in sqlite3, read-only"
	@echo "  make events    - Write data/events.json (published events only)"
	@echo "  make site      - Export a preview and build the Astro site"
	@echo "  make site-dev  - Run the Astro dev server"
	@echo "  make test      - Run tests"
	@echo "  make test-v    - Run tests with per-test output"
	@echo "  make test-race - Run tests under the race detector"
	@echo "  make coverage  - Test coverage report"
	@echo "  make fmt       - Format the Go code"
	@echo "  make vet       - Vet the Go code"
	@echo "  make lint      - Lint the Go code (needs golangci-lint)"
	@echo "  make check     - fmt + vet + test-race"
	@echo "  make clean     - Clean build artifacts"
	@echo "  make help      - Display this help message"
