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

# Pause and resume the cluster without Terraform: both node pools to zero, then back.
# The control plane (free tier), disks and reserved IPs stay. Run from inside the repo
# so direnv supplies the personal gcloud identity.
#
# Autoscaling is switched off before the resize: left on, the autoscaler sees system
# pods go Pending and adds a node straight back. The min/max below MUST match gke.tf,
# or the next `terraform plan` reports drift. While paused, terraform plan shows the
# missing autoscaling as drift; that is expected, so don't apply until gke-up.
CLUSTER := x402
ZONE    := us-central1-a
GKE     := gcloud container clusters

gke-down:
	$(GKE) update $(CLUSTER) --zone $(ZONE) --node-pool workload --no-enable-autoscaling --quiet
	$(GKE) resize $(CLUSTER) --zone $(ZONE) --node-pool workload --num-nodes 0 --quiet
	$(GKE) update $(CLUSTER) --zone $(ZONE) --node-pool system --no-enable-autoscaling --quiet
	$(GKE) resize $(CLUSTER) --zone $(ZONE) --node-pool system --num-nodes 0 --quiet

gke-up:
	$(GKE) resize $(CLUSTER) --zone $(ZONE) --node-pool system --num-nodes 1 --quiet
	$(GKE) update $(CLUSTER) --zone $(ZONE) --node-pool system --enable-autoscaling --min-nodes 1 --max-nodes 2 --quiet
	$(GKE) update $(CLUSTER) --zone $(ZONE) --node-pool workload --enable-autoscaling --min-nodes 0 --max-nodes 3 --quiet
	@echo "Nodes take ~3-5 minutes to join: kubectl get nodes -w"
