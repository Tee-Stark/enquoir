.PHONY: up down test test-integration lint probe

up:
	docker compose -f deploy/docker-compose.yml --env-file .env up -d --wait

down:
	docker compose -f deploy/docker-compose.yml down -v

test:
	go test ./...

test-integration:
	set -a; . ./.env; set +a; go test -tags=integration ./...

lint:
	gofmt -l .
	go vet ./...

probe:
	set -a; . ./.env; set +a; go run ./cmd/probe
