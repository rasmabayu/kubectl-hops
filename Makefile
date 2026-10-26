BIN     := bin/kubectl-hops
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/rasmabayu/kubectl-hops/pkg/cmd.Version=$(VERSION)

.PHONY: build test lint fmt clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/kubectl-hops

test:
	go test -race -cover ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

clean:
	rm -rf bin dist
