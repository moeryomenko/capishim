BINARY := capishim
COVER_FILE ?= coverage.out
RACE_DETECTOR := $(if $(RACE_DETECTOR),-race)
IMPORT_PATH := $(shell go list -m -f {{.Path}} | head -1)

# capishim-specific configuration (REQ-009, REQ-010, REQ-011)
CAPI_SOURCE_REF ?= v1.14.0
CAPISHIM_VERSION ?= v0.1.0
CAPISHIM_STATE_DIR ?= $(HOME)/.local/share/capishim
CAPISHIM_BIND_ADDRESS ?= 127.0.0.1:6443
# Architecture for the built manager/setup binaries (also selects the
# distroless runtime tag in the Containerfiles).
GOARCH ?= amd64
# Pinned stock control-plane images (REQ-011); pulled and re-tagged by `make
# images` so quadlet Image= lines are uniform across the pod.
# etcd: kubeadm's pinned v3.5.x etcd for the k8s v1.36 line. Note: the bare
# 3.5.18 tag does not exist in registry.k8s.io (manifest 404); 3.5.17-0 is the
# v1.36 default and satisfies the spec's "etcd v3.5.x".
ETCD_IMAGE ?= registry.k8s.io/etcd:3.5.17-0
APISERVER_IMAGE ?= registry.k8s.io/kube-apiserver:v1.36.1

.PHONY: default
default: help

.PHONY: build
build: ## Build the capishim binary
	@go build -o $(BINARY) ./cmd/capishim

.PHONY: test
test: ## Run all tests
	@go tool gotestsum --format-hide-empty-pkg -f testname -- $(RACE_DETECTOR) -p=1 -vet=off -count=1 -timeout=1200s -coverprofile=$(COVER_FILE) ./...
	@go tool cover -func=$(COVER_FILE) | grep ^total

.PHONY: cover
cover: test ## Open coverage report in browser
	@go tool cover -html=$(COVER_FILE)

.PHONY: lint
lint: ## Run linter
	@go tool golangci-lint run -v --fix

.PHONY: fmt
fmt: ## Format Go source files
	@gofmt -s -w .
	@git status --short | grep '[A|M]' | grep -E -o "[^ ]*$$" | grep '\.go$$' | xargs -I{} go tool golines --base-formatter=gofumpt --ignore-generated --tab-len=1 --max-len=120 -w {}
	@git status --short | grep '[A|M]' | grep -E -o "[^ ]*$$" | grep '\.go$$' | xargs -I{} go tool goimports -local $(IMPORT_PATH) -w {}

.PHONY: vet
vet: ## Run go vet
	@go vet ./...

.PHONY: tidy
tidy: ## Tidy go module dependencies
	@go mod tidy -v

.PHONY: generate
generate: ## Run go generate
	@go generate ./...

.PHONY: clean
clean: ## Remove build artifacts and coverage output
	@rm -f $(BINARY) $(COVER_FILE)

.PHONY: images
images: ## Build all seven container images (providers from pinned tag)
	@for comp in core cabpk kcp capd setup; do \
		echo "==> building localhost/capishim-$${comp}:$(CAPISHIM_VERSION) (CAPI_SOURCE_REF=$(CAPI_SOURCE_REF))"; \
		podman build \
			--build-arg CAPI_SOURCE_REF="$(CAPI_SOURCE_REF)" \
			--build-arg GOARCH="$(GOARCH)" \
			-t "localhost/capishim-$${comp}:$(CAPISHIM_VERSION)" \
			-f "images/capishim-$${comp}.Containerfile" . || exit 1; \
	done
	# The two control-plane components use pinned stock images (REQ-011): the
	# pulled image is re-tagged under localhost/capishim-* so the quadlet
	# Image= lines are uniform, which makes this target produce all seven
	# images the pod references.
	@podman pull "$(ETCD_IMAGE)"
	@podman tag "$(ETCD_IMAGE)" "localhost/capishim-etcd:$(CAPISHIM_VERSION)"
	@podman pull "$(APISERVER_IMAGE)"
	@podman tag "$(APISERVER_IMAGE)" "localhost/capishim-apiserver:$(CAPISHIM_VERSION)"
	@echo "built $(CAPISHIM_VERSION) capishim images (CAPI_SOURCE_REF=$(CAPI_SOURCE_REF))"

.PHONY: install-quadlet
install-quadlet: ## Render and install quadlet units into the systemd user dir
	@echo "not implemented yet" && exit 1

.PHONY: vendor-templates
vendor-templates: ## Re-vendor in-memory templates from the pinned upstream tag
	@CAPI_SOURCE_REF="$(CAPI_SOURCE_REF)" hack/vendor.sh

.PHONY: check-pins
check-pins: ## Verify upstream pin consistency (go.mod, Containerfiles, provenance)
	@echo "not implemented yet" && exit 1

.PHONY: test-e2e-shim
test-e2e-shim: ## Run the e2e suite against the quadlet pod management cluster
	@echo "not implemented yet" && exit 1

.PHONY: verify-shim
verify-shim: ## Run the full verification flow (VC-01..VC-08)
	@echo "not implemented yet" && exit 1

.PHONY: check
check: lint vet test ## Run lint, vet, and tests (CI gate)

.PHONY: help
help: ## Print this help message
	@echo "Usage: make <target>"
	@echo ""
	@echo "Targets:"
	@grep -F -h '##' $(MAKEFILE_LIST) \
		| grep -F -v fgrep \
		| sort \
		| grep -E '^[a-zA-Z_-]+:.*?## .*$$' \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
