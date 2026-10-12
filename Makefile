# Go side of the repo. The legacy Python app has its own targets under legacy-*.
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/amoghjay/x402_gateway/internal/buildinfo.Version=$(VERSION)
BINARIES := x402-filter
REGISTRY ?= local

.PHONY: build test lint images legacy-test

build:
	@mkdir -p bin
	@for b in $(BINARIES); do \
		CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$$b ./cmd/$$b || exit 1; \
	done

test:
	go test -race ./...

lint:
	golangci-lint run ./...

# One Dockerfile, one image per binary. REGISTRY becomes the Artifact Registry path in Phase 1.
images:
	@for b in $(BINARIES); do \
		docker build --build-arg BINARY=$$b --build-arg VERSION=$(VERSION) \
			-t $(REGISTRY)/$$b:$(VERSION) . || exit 1; \
	done

legacy-test:
	cd legacy/llm-gateway && ruff check . && pytest -q
