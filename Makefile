BINARY := paycli
BUILD_DIR := bin
PKG := github.com/loka-network/paycli
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build install test test-unit test-integration clean fmt vet tidy

all: build

build:
	@mkdir -p $(BUILD_DIR)
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/paycli

install:
	go install $(GOFLAGS) -ldflags "$(LDFLAGS)" ./cmd/paycli

test: test-unit

test-unit:
	go test -race ./pkg/...

test-integration:
	go test -tags=integration -race -v ./tests/...

fmt:
	go fmt ./...

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR)
