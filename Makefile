# Go side of the repo. The legacy Python app has its own targets under legacy-*.
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/amoghjay/x402_gateway/internal/buildinfo.Version=$(VERSION)
BINARIES := x402-filter x402-controller x402-settler
# Built locally, never shipped as an image.
CLIS     := x402-buyer x402-settlebench
REGISTRY ?= local

.PHONY: build generate verify-generate test test-envtest lint verify-radius images legacy-test gke-down gke-up gke-status

build:
	@mkdir -p bin
	@for b in $(BINARIES) $(CLIS); do \
		CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$$b ./cmd/$$b || exit 1; \
	done

# DeepCopy methods and the CRD, from the markers in api/. controller-gen is pinned by the
# `tool` directive in go.mod, so every machine and CI generate the same output.
generate:
	go tool controller-gen object paths=./api/...
	go tool controller-gen crd paths=./api/... output:crd:dir=deploy/k8s/crd

# CI: the committed output must match the markers.
verify-generate: generate
	git diff --exit-code -- api deploy/k8s/crd

test:
	go test -race ./...

# The reconciler is tested against a real kube-apiserver and etcd (envtest), at the
# Kubernetes version the cluster runs. setup-envtest downloads the binaries once; the
# test is skipped when KUBEBUILDER_ASSETS is unset, except in CI.
ENVTEST_K8S  ?= 1.35.0
SETUP_ENVTEST := go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2

test-envtest:
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S) -p path)" go test -race -count=1 -run TestReconciler -v ./internal/controller/

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
