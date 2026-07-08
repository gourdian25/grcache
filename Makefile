# File: Makefile

.PHONY: help build test race coverage coverage-summary bench lint vet fmt staticcheck clean deps

GO := go
MODULE := github.com/gourdian25/grcache
COVERAGE_MIN := 80

help:
	@echo "Makefile targets for grcache:"
	@echo ""
	@echo "  make test             Run all tests"
	@echo "  make race             Run tests with race detector"
	@echo "  make coverage         Generate HTML coverage report"
	@echo "  make coverage-summary Show coverage summary by function"
	@echo "  make bench            Run benchmarks"
	@echo "  make lint             Run linters (requires golangci-lint)"
	@echo "  make vet              Run go vet"
	@echo "  make fmt              Format code"
	@echo "  make clean            Clean build artifacts"
	@echo "  make deps             Verify and tidy dependencies"

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

.DEFAULT_GOAL := help
