GO        ?= go
BIN       ?= bin/pg-rehearse
COMPOSE   := docker compose -f demo/docker-compose.yml
TEST_PG   := pgr-test-postgres
TEST_PORT ?= 55442
TEST_DSN  := postgres://postgres:postgres@localhost:$(TEST_PORT)/pgr_test?sslmode=disable

.PHONY: build test test-integration demo-up demo-down demo-traffic lint fmt golden

build:
	$(GO) build -o $(BIN) ./cmd/pg-rehearse

test:
	$(GO) test ./...

# Starts a throwaway postgres:17, runs the whole suite with PGR_TEST_DSN set, removes it.
test-integration:
	@docker rm -f $(TEST_PG) >/dev/null 2>&1 || true
	docker run -d --rm --name $(TEST_PG) -p $(TEST_PORT):5432 \
		-e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=pgr_test postgres:17 >/dev/null
	@echo "waiting for postgres..."; \
	for i in $$(seq 1 60); do \
		docker exec $(TEST_PG) pg_isready -h 127.0.0.1 -U postgres -d pgr_test >/dev/null 2>&1 && break; \
		sleep 1; \
	done
	PGR_TEST_DSN='$(TEST_DSN)' $(GO) test -count=1 ./...; status=$$?; \
		docker rm -f $(TEST_PG) >/dev/null; exit $$status

demo-up:
	$(COMPOSE) up -d --wait --wait-timeout 300

demo-down:
	$(COMPOSE) down -v

demo-traffic:
	$(GO) run ./demo/traffic -rate 300 -duration 3m

lint:
	$(GO) vet ./...
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@if command -v golangci-lint >/dev/null; then golangci-lint run; else echo "golangci-lint not installed; skipped"; fi

fmt:
	gofmt -w .

# Regenerate report golden files after an intentional rendering change.
golden:
	$(GO) test ./internal/report -update
