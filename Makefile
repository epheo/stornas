VERSION ?= dev
LDFLAGS := -X main.version=$(VERSION)
GOLANGCI_LINT := operator/bin/golangci-lint
BASE_IMAGE ?= ghcr.io/epheo/microshift:latest

.PHONY: ci build generate types lint test web images sync-manifests embed kmod image clean

# The full local gate; .github/workflows/ci.yml runs these same targets.
# Only the image build stays out: it pulls the base image and kernel-devel.
ci: generate build lint test web

build:
	go build -ldflags '$(LDFLAGS)' ./cmd/stornas ./cmd/stornas-agent

generate:
	$(MAKE) -C operator manifests generate

types:
	go run github.com/gzuidhof/tygo@v0.2.21 generate

# One .golangci.yml at the root covers both modules; golangci-lint resolves
# it by walking up from operator/. The pinned binary comes from the operator
# Makefile so both modules lint with the same version. The custom-gcl build
# ignores the module toolchain directive, so pin it to this repo's Go or the
# binary type-checks with an older language version and bails.
lint:
	GOTOOLCHAIN=$$(go env GOVERSION) $(MAKE) -C operator golangci-lint
	"$(GOLANGCI_LINT)" run
	cd operator && "bin/golangci-lint" run

test:
	go test ./...
	$(MAKE) -C operator test

web:
	cd web && npm run format:check && npm run check && npm run build

# App images, built into local podman storage under their runtime names.
images: web
	podman build -f image/app/Containerfile --target server --build-arg VERSION=$(VERSION) -t ghcr.io/epheo/stornas:latest .
	podman build -f image/app/Containerfile --target operator --build-arg VERSION=$(VERSION) -t ghcr.io/epheo/stornas-operator:latest .
	podman build -f image/app/Containerfile --target agent --build-arg VERSION=$(VERSION) -t ghcr.io/epheo/stornas-agent:latest .

sync-manifests: generate
	cp operator/config/crd/bases/*.yaml image/manifests/stornas/crd/

# OCI archives for /usr/lib/embedded-images: the app images from local
# storage plus every ref in image/embedded-images.txt.
embed: images
	mkdir -p image/build/embedded-images
	for img in ghcr.io/epheo/stornas ghcr.io/epheo/stornas-operator ghcr.io/epheo/stornas-agent; do \
		podman save --format oci-archive \
			-o image/build/embedded-images/$$(basename $$img).tar $$img:latest || exit 1; \
	done
	grep -v '^#' image/embedded-images.txt | while read -r img; do \
		[ -z "$$img" ] && continue; \
		out=$$(echo "$$img" | sed 's|.*/||; s|:|-|').tar; \
		skopeo copy docker://$$img \
			oci-archive:image/build/embedded-images/$$out:$$img || exit 1; \
	done

kmod:
	podman build --target kmod -t stornas-kmod -f image/kmod/Containerfile \
		--build-arg KERNEL_VERSION=$$(podman run --rm $(BASE_IMAGE) \
			rpm -q --qf '%{VERSION}-%{RELEASE}.%{ARCH}' kernel-core) \
		image/kmod

image: sync-manifests embed kmod
	podman build --build-context kmod=docker-image://localhost/stornas-kmod \
		--from $(BASE_IMAGE) -f image/Containerfile -t stornas-os:$(VERSION) image

clean:
	rm -f stornas stornas-agent
	rm -rf web/build web/.svelte-kit
