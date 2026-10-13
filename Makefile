IMAGE ?= docker-image-updater
VERSION ?= dev
PKG ?= ./...

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

# End-to-end test against the real Docker daemon and a local registry.
test-integration: build
	IMAGE=$(IMAGE):dev ./test/integration.sh
