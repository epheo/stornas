VERSION ?= dev
LDFLAGS := -X main.version=$(VERSION)
GOLANGCI_LINT := operator/bin/golangci-lint
BASE_IMAGE ?= ghcr.io/epheo/microshift:latest

.PHONY: ci build generate types lint test web images sync-manifests embed kmod image smoke vm-test replication-test upgrade-test clean

# The full local gate; .github/workflows/ci.yml runs these same targets
# and the same stale-generated-files check. The diff is scoped to generated
# paths so a dirty working tree does not fail the gate. Only the image
# build stays out: it pulls the base image and kernel-devel.
ci: generate build lint types test web
	git diff --exit-code -- web/src/lib/model.gen.ts operator/api/v1alpha1/zz_generated.deepcopy.go operator/config

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
	cd web && npm run format:check && npm run check && npm run test && npm run build

# App images, built into local podman storage under their runtime names.
images: web
	podman build -f image/app/Containerfile --target server --build-arg VERSION=$(VERSION) -t ghcr.io/epheo/stornas:latest .
	podman build -f image/app/Containerfile --target operator --build-arg VERSION=$(VERSION) -t ghcr.io/epheo/stornas-operator:latest .
	podman build -f image/app/Containerfile --target agent --build-arg VERSION=$(VERSION) -t ghcr.io/epheo/stornas-agent:latest .

sync-manifests: generate
	cp operator/config/crd/bases/*.yaml image/manifests/stornas/crd/

# OCI archives for /usr/lib/embedded-images: the app images from local
# storage plus every ref in image/embedded-images.txt. The distro's
# import service only imports archives listed in its manifest, so every
# archive gets a "path ref" line in a fragment the Containerfile appends
# to the distro manifest; a tar without a line is dead weight and the
# pod behind it pulls from the network.
embed: images
	mkdir -p image/build/embedded-images
	: > image/build/embedded-images/manifest.stornas
	for img in ghcr.io/epheo/stornas ghcr.io/epheo/stornas-operator ghcr.io/epheo/stornas-agent; do \
		podman save --format oci-archive \
			-o image/build/embedded-images/$$(basename $$img).tar $$img:latest || exit 1; \
		echo "/usr/lib/embedded-images/$$(basename $$img).tar $$img:latest" \
			>> image/build/embedded-images/manifest.stornas; \
	done
	grep -v '^#' image/embedded-images.txt | while read -r img; do \
		[ -z "$$img" ] && continue; \
		out=$$(echo "$$img" | sed 's|.*/||; s|:|-|').tar; \
		skopeo copy --retry-times 3 docker://$$img \
			oci-archive:image/build/embedded-images/$$out:$$img || exit 1; \
		echo "/usr/lib/embedded-images/$$out $$img" \
			>> image/build/embedded-images/manifest.stornas; \
	done

kmod:
	podman build --target kmod -t stornas-kmod -f image/kmod/Containerfile \
		--build-arg KERNEL_VERSION=$$(podman run --rm $(BASE_IMAGE) \
			rpm -q --qf '%{VERSION}-%{RELEASE}.%{ARCH}' kernel-core) \
		image/kmod

image: sync-manifests embed kmod
	podman build --build-context kmod=docker-image://localhost/stornas-kmod \
		--from $(BASE_IMAGE) -f image/Containerfile -t stornas-os:$(VERSION) image

# Runtime gates, same harness shapes as the distro's smoke-test and
# vm-test. Both need a root-capable podman (PODMAN='sudo podman');
# vm-test also needs qemu-system-x86_64 and OVMF. smoke boots the image
# as a privileged container in minutes; vm-test is the full VM boot.
PODMAN ?= podman
smoke:
	IMAGE=localhost/stornas-os:$(VERSION) PODMAN="$(PODMAN)" ./hack/smoke-test.sh

vm-test:
	IMAGE=localhost/stornas-os:$(VERSION) PODMAN="$(PODMAN)" ./hack/boot-test.sh

# Two VMs on a shared bridge; microshift multinode join; a replicated
# PVC surviving peer loss and resyncing; target and share failover with
# the VIP moving and the returned node fenced. Heaviest gate.
replication-test:
	IMAGE=localhost/stornas-os:$(VERSION) PODMAN="$(PODMAN)" ./hack/replication-test.sh

upgrade-test:
	IMAGE=localhost/stornas-os:$(VERSION) PODMAN="$(PODMAN)" ./hack/upgrade-test.sh

clean:
	rm -f stornas stornas-agent
	rm -rf web/build web/.svelte-kit operator/bin image/build
