VERSION ?= dev
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test web operator image clean

build:
	go build -ldflags '$(LDFLAGS)' ./cmd/stornas ./cmd/stornas-agent

test:
	go test ./...
	$(MAKE) -C operator test

web:
	cd web && npm run check && npm run build

operator:
	$(MAKE) -C operator manifests generate build

image:
	podman build -f image/Containerfile -t stornas:$(VERSION) .

clean:
	rm -f stornas stornas-agent
	rm -rf web/build web/.svelte-kit
