.PHONY: build test help

build:
	CGO_ENABLED=0 go build -o bin/migration .

test:
	go test ./...

help: build
	./bin/migration --help
