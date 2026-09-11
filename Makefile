.PHONY: build test lint check run stop redis loadtest clean

BIN := bin/crossbar

build:
	go build -o $(BIN) ./cmd/crossbar

# Limiter and budget tests need a Redis; they skip if none is reachable.
# `make redis` starts one on localhost:6379.
test:
	go test -race ./...

redis:
	docker compose -f deploy/docker-compose.yml up -d redis

lint:
	go vet ./...
	golangci-lint run

check: lint test

run:
	docker compose -f deploy/docker-compose.yml up --build

stop:
	docker compose -f deploy/docker-compose.yml down

loadtest:
	go run ./loadtest -target http://localhost:8080

clean:
	rm -rf bin coverage.out
