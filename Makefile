.PHONY: setup dev migrate setup-max-webhook test test-race vet lint-tool lint-install lint lint-config vuln-tool vuln-install vuln ci build docker-build compose-config compose-up compose-down compose-logs clean

GOLANGCI_LINT := $(shell if test -x ./bin/golangci-lint; then echo ./bin/golangci-lint; else command -v golangci-lint 2>/dev/null || echo golangci-lint; fi)
GOVULNCHECK_VERSION := v1.6.0
GOVULNCHECK := ./bin/govulncheck
GOVULNCHECK_DB ?= https://vuln.go.dev
COMPOSE := $(shell if docker compose version >/dev/null 2>&1; then echo "docker compose"; else echo "docker-compose"; fi)

setup:
	@test -f .env || cp .env.example .env
	@echo "Создан backend/.env. Заполните пароли PostgreSQL и данные приложения Яндекс ID."

dev:
	@set -a; test ! -f .env || . ./.env; set +a; \
		if test -z "$$DATABASE_URL"; then echo "DATABASE_URL обязателен для запуска без Docker Compose" >&2; exit 1; fi; \
		go run ./cmd/server

migrate:
	@set -a; test ! -f .env || . ./.env; set +a; \
		if test -z "$$DIRECT_DATABASE_URL"; then echo "DIRECT_DATABASE_URL обязателен для миграций" >&2; exit 1; fi; \
		go run ./cmd/migrate

setup-max-webhook:
	@set -a; test ! -f .env || . ./.env; set +a; \
		go run ./cmd/setup-max-webhook

test:
	@set -a; test ! -f .env || . ./.env; set +a; \
		if test -z "$$TEST_DATABASE_URL"; then echo "TEST_DATABASE_URL обязателен; тесты используют PostgreSQL" >&2; exit 1; fi; \
		go test ./...

test-race:
	@set -a; test ! -f .env || . ./.env; set +a; \
		if test -z "$$TEST_DATABASE_URL"; then echo "TEST_DATABASE_URL обязателен; тесты используют PostgreSQL" >&2; exit 1; fi; \
		go test -race ./...

vet:
	go vet ./...

lint-install:
	./scripts/install-golangci-lint.sh

lint-tool:
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || (echo "golangci-lint v2 is required; run 'make lint-install'" >&2; exit 1)

lint: lint-tool
	$(GOLANGCI_LINT) run ./...

lint-config: lint-tool
	$(GOLANGCI_LINT) config verify

vuln-install:
	mkdir -p bin
	GOBIN="$(CURDIR)/bin" go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

vuln-tool:
	@test -x $(GOVULNCHECK) || (echo "govulncheck $(GOVULNCHECK_VERSION) is required; run 'make vuln-install'" >&2; exit 1)
	@$(GOVULNCHECK) -version | grep -Fq "Scanner: govulncheck@$(GOVULNCHECK_VERSION)" || (echo "govulncheck version mismatch; run 'make vuln-install'" >&2; exit 1)

vuln: vuln-tool
	$(GOVULNCHECK) -db=$(GOVULNCHECK_DB) ./...

ci: lint test-race vet vuln

build:
	mkdir -p bin
	go build -trimpath -o bin/maxpilot ./cmd/server
	go build -trimpath -o bin/migrate ./cmd/migrate
	go build -trimpath -o bin/setup-max-webhook ./cmd/setup-max-webhook

docker-build:
	docker build -t max-studio-backend:local .

compose-config:
	$(COMPOSE) config -q

compose-up:
	$(COMPOSE) up --build -d

compose-down:
	$(COMPOSE) down

compose-logs:
	$(COMPOSE) logs -f backend migrate pgbouncer postgres

clean:
	rm -rf bin coverage.out
