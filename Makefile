.PHONY: build test help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.2.0)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-X github.com/rustfs/migration/cmd.Version=$(VERSION)" -o bin/migration .

test:
	go test ./...

help: build
	./bin/migration --help
