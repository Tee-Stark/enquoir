.PHONY: up down test test-integration lint probe sandbox-setup worker demo api chaos e2e

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

sandbox-setup:
	set -a; . ./.env; set +a; go run ./cmd/sandboxsetup

worker:
	set -a; . ./.env; set +a; go run ./cmd/worker

demo:
	set -a; . ./.env; set +a; go run ./cmd/demo

api:
	set -a; . ./.env; set +a; go run ./cmd/api

chaos:
	set -a; . ./.env; set +a; go run ./cmd/chaos

e2e:
	scripts/e2e.sh
