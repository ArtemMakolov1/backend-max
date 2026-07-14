.PHONY: setup dev test test-race vet build docker-build compose-config clean

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

build:
	mkdir -p bin
	go build -trimpath -o bin/maxpilot ./cmd/server

docker-build:
	docker build -t max-studio-backend:local .

compose-config:
	docker compose config --quiet

clean:
	rm -rf bin coverage.out
