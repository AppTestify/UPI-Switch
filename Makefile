TEST_DATABASE_URL ?= postgres://postgres:postgres@localhost:5432/upiswitch_test?sslmode=disable

.PHONY: build test e2e lint run demo up down

build:
	go build -o bin/ ./cmd/...

test:
	go test ./internal/...

e2e:
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race -count=1 ./test/e2e/

lint:
	gofmt -l . | (! grep .) && go vet ./...

run:
	./scripts/run-local.sh

demo:
	go run ./cmd/demo

up:
	docker compose up --build -d

down:
	docker compose down
