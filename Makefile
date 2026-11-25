.PHONY: all build web build-controller build-worker build-all test test-race test-e2e lint vuln clean dev-controller dev-worker

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

all: web build

web:
	cd web && NG_CLI_ANALYTICS=false npm run build
	rm -rf internal/controller/webdist/*
	cp -R web/dist/syre-web/browser/. internal/controller/webdist/
	touch internal/controller/webdist/.gitkeep

build-controller:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/syre ./cmd/syre

build-worker:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/syred ./cmd/syred

build: build-controller build-worker

build-all: web
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/syre-linux-amd64 ./cmd/syre
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/syre-linux-arm64 ./cmd/syre
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/syre-darwin-arm64 ./cmd/syre
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/syred-linux-amd64 ./cmd/syred
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/syred-linux-arm64 ./cmd/syred
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/syred-darwin-arm64 ./cmd/syred
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/syred-darwin-amd64 ./cmd/syred
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/syred-windows-amd64.exe ./cmd/syred

test:
	go test ./...

test-race:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal; exit 1)
	go mod tidy -diff
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...

test-e2e: all
	cd web && npm run test:e2e

dev-controller:
	go run ./cmd/syre serve --no-auth

dev-worker:
	go run ./cmd/syred run --enable-mock-driver --controller ws://127.0.0.1:8420/ws/worker

clean:
	rm -rf bin dist web/dist internal/controller/webdist/*
	touch internal/controller/webdist/.gitkeep
