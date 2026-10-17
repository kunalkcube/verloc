# verloc — build, test, run
.PHONY: all build test vet fmt tidy dash clean version run

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
