.PHONY: up down test test-integration lint

up:
	docker compose -f deploy/docker-compose.yml --env-file .env up -d --wait

down:
	docker compose -f deploy/docker-compose.yml down -v

test:
	go test ./...

test-integration:
	go test -tags=integration ./...

lint:
	gofmt -l .
	go vet ./...
