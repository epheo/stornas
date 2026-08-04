VERSION ?= dev
LDFLAGS := -X main.version=$(VERSION)
GOLANGCI_LINT := operator/bin/golangci-lint
BASE_IMAGE ?= ghcr.io/epheo/microshift:latest

.PHONY: ci build generate lint test web image clean

# The full local gate; .github/workflows/ci.yml runs these same targets.
# Only the image build stays out: it pulls the base image and kernel-devel.
ci: generate build lint test web

build:
	go build -ldflags '$(LDFLAGS)' ./cmd/stornas ./cmd/stornas-agent

generate:
	$(MAKE) -C operator manifests generate

# One .golangci.yml at the root covers both modules; golangci-lint resolves
# it by walking up from operator/. The pinned binary comes from the operator
# Makefile so both modules lint with the same version.
lint:
	$(MAKE) -C operator golangci-lint
	"$(GOLANGCI_LINT)" run
	cd operator && "bin/golangci-lint" run

test:
	go test ./...
	$(MAKE) -C operator test

web:
	cd web && npm run format:check && npm run check && npm run build

image:
	podman build --target kmod -t stornas-kmod -f image/kmod/Containerfile \
		--build-arg KERNEL_VERSION=$$(podman run --rm $(BASE_IMAGE) \
			rpm -q --qf '%{VERSION}-%{RELEASE}.%{ARCH}' kernel-core) \
		image/kmod
	podman build --build-context kmod=docker-image://localhost/stornas-kmod \
		--from $(BASE_IMAGE) -f image/Containerfile -t stornas:$(VERSION) image

clean:
	rm -f stornas stornas-agent
	rm -rf web/build web/.svelte-kit
