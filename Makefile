.PHONY: build test help build-linux build-linux-amd64 build-linux-arm64

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -o bin/migration .

# Linux x86_64. CGO_ENABLED=0 keeps the pure-Go SQLite build static.
build-linux-amd64:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/migration-linux-amd64 .

# Linux arm64 / aarch64. No C cross-compiler: CGO stays off.
build-linux-arm64:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/migration-linux-arm64 .

build-linux: build-linux-amd64 build-linux-arm64

test:
	go test ./...

help: build
	./bin/migration --help
