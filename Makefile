# File: Makefile

.PHONY: help build test race coverage coverage-summary coverage-check bench lint vet fmt staticcheck clean deps tag release goreleaser-check guard-version

GO := go
MODULE := github.com/gourdian25/grcache
COVERAGE_MIN := 80
VERSION ?=

help:
	@echo "Makefile targets for grcache:"
	@echo ""
	@echo "  make test             Run all tests"
	@echo "  make race             Run tests with race detector"
	@echo "  make coverage         Generate HTML coverage report"
	@echo "  make coverage-summary Show coverage summary by function"
	@echo "  make coverage-check   Check each package meets the $(COVERAGE_MIN)% threshold"
	@echo "  make bench            Run benchmarks"
	@echo "  make lint             Run linters (requires golangci-lint)"
	@echo "  make vet              Run go vet"
	@echo "  make fmt              Format code"
	@echo "  make clean            Clean build artifacts"
	@echo "  make deps             Verify and tidy dependencies"
	@echo "  make tag VERSION=vX.Y.Z         Create and push a git tag"
	@echo "  make release VERSION=vX.Y.Z     Tag, push, and run goreleaser release --clean"
	@echo "  make goreleaser-check           Dry run: validate config + snapshot release (no tag/push)"

test:
	@echo "Running tests..."
	$(GO) test -count=1 -timeout=5m -cover ./...
	@echo "✓ Tests passed"

race:
	@echo "Running tests with race detector..."
	$(GO) test -race -timeout 5m ./...
	@echo "✓ Race detector tests passed"

coverage:
	@echo "Generating coverage report..."
	$(GO) test -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "✓ HTML coverage report saved as coverage.html"

coverage-summary:
	@echo "Coverage summary by function:"
	@$(GO) test -coverprofile=coverage.out ./...
	@$(GO) tool cover -func=coverage.out

# Requires local Redis/Postgres/Mongo/memcached (see README) — every
# non-test-helper package must independently meet COVERAGE_MIN, matching
# gourdiantoken's own coverage-check convention. The conformance package is
# test-only infrastructure (no _test.go files of its own) and is skipped.
coverage-check:
	@echo "Checking each package meets $(COVERAGE_MIN)% coverage..."
	@fail=0; \
	for pkg in . ./memory ./redis ./memcached ./postgres ./mongo; do \
		out=$$($(GO) test -cover $$pkg 2>&1); \
		pct=$$(echo "$$out" | grep -o '[0-9.]*%' | tr -d '%'); \
		if [ -z "$$pct" ]; then echo "✗ $$pkg: no coverage output"; fail=1; continue; fi; \
		below=$$(awk -v p="$$pct" -v m="$(COVERAGE_MIN)" 'BEGIN { print (p < m) ? 1 : 0 }'); \
		if [ "$$below" = "1" ]; then \
			echo "✗ $$pkg: $$pct% is below $(COVERAGE_MIN)% threshold"; fail=1; \
		else \
			echo "✓ $$pkg: $$pct%"; \
		fi; \
	done; \
	exit $$fail

bench:
	@echo "Running benchmarks..."
	$(GO) test -bench=. -benchmem -benchtime=10s ./...
	@echo "✓ Benchmarks complete"

lint:
	@echo "Running linters..."
	@which golangci-lint > /dev/null || (echo "golangci-lint not found. Install with: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest" && exit 1)
	golangci-lint run ./...
	@echo "✓ Linting passed"

vet:
	@echo "Running go vet..."
	$(GO) vet ./...
	@echo "✓ Vet analysis complete"

fmt:
	@echo "Formatting code..."
	$(GO) fmt ./...
	@echo "✓ Code formatted"

clean:
	@echo "Cleaning build artifacts..."
	rm -f coverage.out coverage.html
	$(GO) clean ./...
	@echo "✓ Clean complete"

deps:
	@echo "Verifying dependencies..."
	$(GO) mod verify
	@echo "Tidying dependencies..."
	$(GO) mod tidy
	@echo "✓ Dependency verification complete"

guard-version:
	@if [ -z "$(VERSION)" ]; then \
		echo "❌ VERSION is required (example: make release VERSION=v0.1.0)"; \
		exit 1; \
	fi

tag: guard-version
	@echo "Tagging $(VERSION)..."
	git tag $(VERSION)
	git push origin $(VERSION)
	@echo "✓ Tagged and pushed $(VERSION)"

release: guard-version tag
	@echo "Releasing $(VERSION) with goreleaser..."
	@which goreleaser > /dev/null || (echo "goreleaser not found. Install with: go install github.com/goreleaser/goreleaser/v2@latest" && exit 1)
	goreleaser release --clean
	@echo "✓ Released $(VERSION)"

# Dry run: validates .goreleaser.yaml and builds a snapshot release locally
# without requiring a git tag or pushing anything.
goreleaser-check:
	@which goreleaser > /dev/null || (echo "goreleaser not found. Install with: go install github.com/goreleaser/goreleaser/v2@latest" && exit 1)
	goreleaser check
	goreleaser release --snapshot --clean

.DEFAULT_GOAL := help
