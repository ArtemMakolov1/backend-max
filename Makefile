.PHONY: setup dev test test-race vet lint-tool lint-install lint lint-config ci build docker-build compose-config clean

GOLANGCI_LINT := $(shell if test -x ./bin/golangci-lint; then echo ./bin/golangci-lint; else command -v golangci-lint 2>/dev/null || echo golangci-lint; fi)

setup:
	@test -f .env || cp .env.example .env
	@echo "Backend environment created. Add secrets only to .env."

dev:
	@set -a; test ! -f .env || . ./.env; set +a; go run ./cmd/server

test:
	go test ./...

test-race:
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

ci: lint test-race vet

build:
	mkdir -p bin
	go build -trimpath -o bin/maxpilot ./cmd/server

docker-build:
	docker build -t max-studio-backend:local .

compose-config:
	docker compose config --quiet

clean:
	rm -rf bin coverage.out
