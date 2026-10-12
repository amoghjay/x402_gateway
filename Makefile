# Go side of the repo. The legacy Python app has its own targets under legacy-*.
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/amoghjay/x402_gateway/internal/buildinfo.Version=$(VERSION)
BINARIES := x402-filter
REGISTRY ?= local

.PHONY: build test lint images legacy-test gke-down gke-up

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

# Pause and resume the cluster through Terraform, so state never drifts from reality.
# Paused: both node pools at zero; control plane (free tier), disks, IPs stay.
# Run from inside the repo so direnv supplies the personal identity.
TF := terraform -chdir=deploy/terraform

gke-down:
	$(TF) apply -var paused=true

gke-up:
	$(TF) apply -var paused=false
	@echo "Nodes take ~3-5 minutes to join: kubectl get nodes -w"
