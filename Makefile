IMAGE ?= docker-image-updater
VERSION ?= dev
PKG ?= ./...
DIND_IMAGE ?= docker:27-dind
DIND_ARGS ?=

.PHONY: test test-pkg build test-integration

# Full unit test suite, executed inside Docker. The `test` stage is never cached.
test:
	docker build --target test --no-cache-filter test --progress=plain .

# Fast feedback loop for a single package, e.g. make test-pkg PKG=./internal/config/...
test-pkg:
	docker run --rm -v "$(CURDIR)":/src -w /src -e CGO_ENABLED=0 \
		-v diu-gocache:/root/.cache/go-build golang:1.23-alpine go test $(PKG) -v

build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):dev .

# End-to-end test inside an isolated docker:dind daemon (never the developer's Docker daemon).
# DIND_IMAGE picks the daemon (docker:27-dind: classic image store, docker:29-dind: containerd store).
test-integration: build
	IMAGE=$(IMAGE):dev DIND_IMAGE="$(DIND_IMAGE)" DIND_ARGS="$(DIND_ARGS)" ./test/integration.sh
