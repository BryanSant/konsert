.PHONY: all build web build-controller build-worker build-all test test-race clean dev-controller dev-worker

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

all: web build

web:
	cd web && NG_CLI_ANALYTICS=false npm run build
	rm -rf internal/controller/webdist/*
	cp -R web/dist/konsert-web/browser/. internal/controller/webdist/
	touch internal/controller/webdist/.gitkeep

build-controller:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/konsert ./cmd/konsert

build-worker:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/konsertd ./cmd/konsertd

build: build-controller build-worker

build-all: web
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/konsert-linux-amd64 ./cmd/konsert
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/konsert-linux-arm64 ./cmd/konsert
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/konsert-darwin-arm64 ./cmd/konsert
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/konsertd-linux-amd64 ./cmd/konsertd
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/konsertd-linux-arm64 ./cmd/konsertd
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/konsertd-darwin-arm64 ./cmd/konsertd
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/konsertd-darwin-amd64 ./cmd/konsertd
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/konsertd-windows-amd64.exe ./cmd/konsertd

test:
	go test ./...

test-race:
	go test -race ./...

test-e2e: all
	cd web && npm run test:e2e

dev-controller:
	go run ./cmd/konsert serve --no-auth --listen :8420

dev-worker:
	go run ./cmd/konsertd run --enable-mock-driver --controller ws://127.0.0.1:8420/ws/worker

clean:
	rm -rf bin dist web/dist internal/controller/webdist/*
	touch internal/controller/webdist/.gitkeep
