SHELL := /bin/bash

# Requires the bundled toolchain: run scripts/bootstrap-toolchain.sh first,
# then every make target sources .toolchain/env.sh automatically.
ENV := source .toolchain/env.sh &&

LDFLAGS := -s -w

.PHONY: build build-linux build-windows build-linux-arm64 build-all \
        test vet fmt run clean

build-linux:
	$(ENV) go build -trimpath -ldflags "$(LDFLAGS)" -o dist/truedns-linux-amd64 ./cmd/truedns

build-windows:
	$(ENV) GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o dist/truedns-windows-amd64.exe ./cmd/truedns

build-linux-arm64:
	$(ENV) GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o dist/truedns-linux-arm64 ./cmd/truedns

build: build-linux build-windows

build-all: build-linux build-windows build-linux-arm64

test:
	$(ENV) go test ./...

vet:
	$(ENV) go vet ./...

fmt:
	$(ENV) gofmt -w $$(find cmd internal -name '*.go')

run:
	$(ENV) go run ./cmd/truedns

clean:
	rm -rf dist
