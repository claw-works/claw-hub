.PHONY: build build-agent test run docker-up docker-down clean

BINARY=claw-hub
AGENT_BINARY=claw-hub-agent
CMD_DIR=./cmd/server
AGENT_CMD_DIR=./cmd/agent

## build: compile the server binary
build:
	go build -o $(BINARY) $(CMD_DIR)

## build-agent: compile the claw-hub-agent binary
build-agent:
	go build -o $(AGENT_BINARY) $(AGENT_CMD_DIR)

## build-linux-arm64: cross-compile server + agent for Linux ARM64
build-linux-arm64:
	GOOS=linux GOARCH=arm64 go build -o $(BINARY)-linux-arm64 $(CMD_DIR)
	GOOS=linux GOARCH=arm64 go build -o $(AGENT_BINARY)-linux-arm64 $(AGENT_CMD_DIR)

## test: run all tests
test:
	go test ./...

## test-verbose: run tests with verbose output
test-verbose:
	go test -v ./...

## run: run the server locally (requires docker-up for DB)
run: build
	./$(BINARY)

## docker-up: start PostgreSQL and MongoDB via docker-compose
docker-up:
	docker compose up -d
	@echo "Waiting for databases to be ready..."
	@sleep 3
	@docker compose ps

## docker-down: stop and remove containers
docker-down:
	docker compose down

## docker-logs: tail container logs
docker-logs:
	docker compose logs -f

## clean: remove build artifacts
clean:
	rm -f $(BINARY) $(BINARY)-linux-arm64

## help: show this help
help:
	@grep -E '^## ' Makefile | sed 's/## //'
