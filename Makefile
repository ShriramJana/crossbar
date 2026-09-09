.PHONY: build test lint check run stop loadtest clean

BIN := bin/relay

build:
	go build -o $(BIN) ./cmd/relay

test:
	go test -race ./...

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
