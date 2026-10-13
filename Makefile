BINARY  := ghome
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BUILD   := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: help build run test race vet fmt tidy check ui-check cross clean

help:
	@echo "make build     - build bin/ghome for this machine"
	@echo "make run       - run against ./nav.yaml"
	@echo "make cross     - build linux/amd64, linux/arm64 and darwin/arm64 into dist/"
	@echo "make check     - fmt + vet + test"
	@echo "make ui-check  - drive the UI in headless Chrome (needs a running ghome)"
	@echo "make clean     - remove build output"

build:
	@mkdir -p bin
	$(BUILD) -o bin/$(BINARY) .

run:
	go run . --source=file --file=./nav.yaml

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

check: fmt vet test

# The frontend has no build step and no Go tests, so this is where UI
# regressions get caught. Point it at a running instance:
#   make run &            # or any ghome
#   make ui-check
#   make ui-check UI_URL=http://127.0.0.1:8080/
UI_URL ?= http://127.0.0.1:8080/
ui-check:
	@node scripts/ui-check.mjs $(UI_URL)

# Static binaries for a server whose architecture you do not want to think
# about: drop one on the box and run it, no runtime, no libc.
cross:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 $(BUILD) -o dist/$(BINARY)-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 $(BUILD) -o dist/$(BINARY)-linux-arm64 .
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 $(BUILD) -o dist/$(BINARY)-darwin-arm64 .
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 $(BUILD) -o dist/$(BINARY)-darwin-amd64 .
	@ls -lh dist

clean:
	rm -rf bin dist
