# File: Makefile

.PHONY: help build test race coverage coverage-summary coverage-check bench lint vet fmt staticcheck clean deps docker-up docker-down tag release goreleaser-check guard-version

GO := go
MODULE := github.com/gourdian25/grcache
COVERAGE_MIN := 95
VERSION ?=

help:
	@echo "Makefile targets for grcache:"
	@echo ""
	@echo "  make test             Run all tests"
	@echo "  make race             Run tests with race detector"
	@echo "  make coverage         Generate HTML coverage report"
	@echo "  make coverage-summary Show coverage summary by function"
	@echo "  make coverage-check   Check the root package meets the $(COVERAGE_MIN)% threshold"
	@echo "  make bench            Run benchmarks"
	@echo "  make lint             Run linters (requires golangci-lint)"
	@echo "  make vet              Run go vet"
	@echo "  make fmt              Format code"
	@echo "  make clean            Clean build artifacts"
	@echo "  make deps             Verify and tidy dependencies"
	@echo "  make docker-up        Start the shared Postgres/Redis/Mongo/Memcached test containers (idempotent)"
	@echo "  make docker-down      Stop those containers (state preserved for a fast restart)"
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

# Requires local Redis/Postgres/Mongo/memcached (see README) — grcache is
# now a flat, single-package repo (see CLAUDE.md), so only the root package
# is checked; example/ is a runnable demo, not library code under test.
# (Previously iterated over one directory per backend subpackage, including
# a stale "./mongo" entry that never matched the actual "mongostore"
# directory name and so silently reported "no coverage output" for it on
# every run — moot now that there's only one package to measure.)
coverage-check:
	@echo "Checking the root package meets $(COVERAGE_MIN)% coverage..."
	@out=$$($(GO) test -cover . 2>&1); \
	pct=$$(echo "$$out" | grep -o '[0-9.]*%' | tr -d '%'); \
	if [ -z "$$pct" ]; then echo "✗ no coverage output"; exit 1; fi; \
	below=$$(awk -v p="$$pct" -v m="$(COVERAGE_MIN)" 'BEGIN { print (p < m) ? 1 : 0 }'); \
	if [ "$$below" = "1" ]; then \
		echo "✗ $$pct% is below $(COVERAGE_MIN)% threshold"; exit 1; \
	else \
		echo "✓ $$pct%"; \
	fi

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

# docker-up is idempotent: safe to run repeatedly, and safe to run
# alongside grnoti/graudit/gourdiantoken's own `make docker-up` since every
# gourdian25 repo shares these same container names/ports — each just
# gets its own database/keyspace/DB-index inside them (see CLAUDE.md).
# grcache doesn't need Kafka, unlike grnoti; it's the only repo needing
# Memcached.
docker-up:
	@echo "Starting shared test containers..."
	@docker inspect gourdian-postgres >/dev/null 2>&1 || docker run -d --name gourdian-postgres -p 5432:5432 \
		-e POSTGRES_USER=postgres_user -e POSTGRES_PASSWORD=postgres_password -e POSTGRES_DB=grcache_test postgres:16
	@docker start gourdian-postgres >/dev/null 2>&1 || true
	@docker inspect gourdian-redis >/dev/null 2>&1 || docker run -d --name gourdian-redis -p 6379:6379 redis:7 --requirepass redis_password
	@docker start gourdian-redis >/dev/null 2>&1 || true
	@docker inspect gourdian-memcached >/dev/null 2>&1 || docker run -d --name gourdian-memcached -p 11211:11211 memcached:1.6
	@docker start gourdian-memcached >/dev/null 2>&1 || true
	@docker volume create gourdian-mongo-keyfile >/dev/null
	@docker inspect gourdian-mongo-auth >/dev/null 2>&1 || (docker run --rm -v gourdian-mongo-keyfile:/keyfile-dir mongo:7 bash -c "openssl rand -base64 756 > /keyfile-dir/mongo-keyfile && chmod 400 /keyfile-dir/mongo-keyfile && chown 999:999 /keyfile-dir/mongo-keyfile" && docker run -d --name gourdian-mongo-auth -p 27018:27017 -e MONGO_INITDB_ROOT_USERNAME=root -e MONGO_INITDB_ROOT_PASSWORD=mongo_password -v gourdian-mongo-keyfile:/etc/mongo-keyfile-dir mongo:7 --replSet rs0 --keyFile /etc/mongo-keyfile-dir/mongo-keyfile)
	@docker start gourdian-mongo-auth >/dev/null 2>&1 || true
	@echo "Waiting for Postgres..."
	@until docker exec gourdian-postgres pg_isready -U postgres_user >/dev/null 2>&1; do sleep 1; done
	@docker exec gourdian-postgres psql -U postgres_user -d postgres -tc "SELECT 1 FROM pg_database WHERE datname = 'grcache_test'" | grep -q 1 || \
		docker exec gourdian-postgres psql -U postgres_user -d postgres -c "CREATE DATABASE grcache_test"
	@echo "Waiting for Redis..."
	@until docker exec gourdian-redis redis-cli -a redis_password ping 2>/dev/null | grep -q PONG; do sleep 1; done
	@echo "Waiting for Memcached..."
	@until (printf 'version\r\n' | nc -w 1 localhost 11211 2>/dev/null | grep -q VERSION); do sleep 1; done
	@echo "Waiting for Mongo (auth + replica set)..."
	@until docker exec gourdian-mongo-auth mongosh --quiet -u root -p mongo_password --authenticationDatabase admin --eval 'db.runCommand({ping:1})' >/dev/null 2>&1; do sleep 1; done
	@docker exec gourdian-mongo-auth mongosh --quiet -u root -p mongo_password --authenticationDatabase admin --eval 'rs.initiate()' >/dev/null 2>&1 || true
	@echo "Docker test infrastructure ready (postgres/redis/mongo-auth/memcached)"

docker-down:
	@docker stop gourdian-postgres gourdian-redis gourdian-memcached gourdian-mongo-auth 2>/dev/null || true
	@echo "Stopped (containers preserved for a fast restart via 'make docker-up')"

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
