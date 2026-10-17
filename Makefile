# verloc — build, test, run, bench
.PHONY: all build test vet fmt tidy dash clean version run bench bench-scale bench-race race check

BENCH      ?= .
BENCHTIME  ?= 200x
OUT        ?= verloc
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)

all: test build

build: dash
	CGO_ENABLED=0 go build -trimpath -ldflags='$(LDFLAGS)' -o $(OUT) ./cmd/verloc

dash:
	cd dashboard && npm run build

test:
	go test ./...

# Fast benchmark set: proxy hot path + telemetry. -short skips the DB probes.
bench:
	go test ./internal/server/ -short -run='^$$' -bench='$(BENCH)' -benchmem -benchtime=$(BENCHTIME) -count=3

# Adds the SQLite scaling probes (seeds synthetic history; slower).
bench-scale:
	go test ./internal/server/ -run='^$$' -bench='$(BENCH)' -benchmem -benchtime=$(BENCHTIME) -count=1
	go test ./internal/server/ -run='TestQueryPlans' -v -count=1

# Race detector over the full suite — the concurrency regression net.
race:
	go test -race ./... -count=1

bench-race:
	go test ./internal/server/ -short -race -run='^$$' -bench='$(BENCH)' -benchmem -benchtime=$(BENCHTIME) -count=1

# What CI runs.
check: vet race

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

tidy:
	go mod tidy

version: build
	./$(OUT) -version

run: build
	./$(OUT) -config config.json

clean:
	rm -f $(OUT)
	rm -rf internal/server/static/assets
