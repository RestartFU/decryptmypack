.PHONY: build build-linux test dev

DEPLOY_BIN ?= decryptmypack
DEPLOY_ARTIFACT ?= decryptmypack-linux-amd64

build:
	env GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go-mod go build -trimpath -o $(DEPLOY_BIN) ./cmd

build-linux:
	env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go-mod go build -trimpath -o $(DEPLOY_ARTIFACT) ./cmd

test:
	env GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go-mod go test ./...

dev:
	LISTEN_ADDR=:8080 DATA_DIR=./data TOKEN_PATH=./token.json go run ./cmd
