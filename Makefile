# Go side of the repo. The legacy Python app has its own targets under legacy-*.
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/amoghjay/x402_gateway/internal/buildinfo.Version=$(VERSION)
BINARIES := x402-filter
REGISTRY ?= local

.PHONY: build test lint verify-radius images legacy-test gke-down gke-up gke-status

build:
	@mkdir -p bin
	@for b in $(BINARIES); do \
		CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$$b ./cmd/$$b || exit 1; \
	done

test:
	go test -race ./...

lint:
	golangci-lint run ./...

# Checks every constant in internal/radius against both live networks (RPC, SBC's
# EIP-712 domain and EIP-3009, facilitators, explorers, faucet). Needs the internet.
verify-radius:
	go test -tags live -count=1 -run Live -v ./internal/radius

# One Dockerfile, one image per binary. REGISTRY becomes the Artifact Registry path in Phase 1.
images:
	@for b in $(BINARIES); do \
		docker build --build-arg BINARY=$$b --build-arg VERSION=$(VERSION) \
			-t $(REGISTRY)/$$b:$(VERSION) . || exit 1; \
	done

legacy-test:
	cd legacy/llm-gateway && ruff check . && pytest -q

# Pause and resume the cluster without Terraform: both node pools to zero, then back.
# The control plane (free tier) and reserved IPs stay. Run from inside the repo so direnv
# supplies the personal gcloud identity. The steps, the autoscaler settle wait and the
# verify-and-retry live in tools/gke/pause.sh.
#
# While paused, terraform plan shows the missing autoscaling as drift; that is expected,
# so don't apply until gke-up.
gke-down:
	@tools/gke/pause.sh down

gke-up:
	@tools/gke/pause.sh up

gke-status:
	@tools/gke/pause.sh status
