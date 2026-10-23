#!/usr/bin/env bash
# Runs the tests in Docker, then builds the linux/amd64 + linux/arm64 image and pushes it to the private registry
# as :latest, plus :<version> when HEAD is on a v* tag (the tag without its `v` prefix).
#
#   ./scripts/publish.sh                 # tests + build + push
#   SKIP_TESTS=1 ./scripts/publish.sh    # build + push only
set -euo pipefail

cd "$(dirname "$0")/.."

REGISTRY="${REGISTRY:-registry.bldwebagency.fr}"
PROJECT="${PROJECT:-docker-image-updater}"
IMAGE="${REGISTRY}/${PROJECT}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"

version=""
if tag="$(git describe --tags --exact-match --match 'v*' HEAD 2>/dev/null)"; then
  version="${tag#v}"
fi

if [[ -z "${SKIP_TESTS:-}" ]]; then
  make test
  make test-integration
fi

tags=(-t "${IMAGE}:latest")
if [[ -n "$version" ]]; then
  tags+=(-t "${IMAGE}:${version}")
fi

docker buildx build \
  --platform "${PLATFORMS}" \
  --build-arg "VERSION=${version:-dev}" \
  --provenance=false \
  "${tags[@]}" \
  --push .

echo "Pushed ${IMAGE}:latest${version:+ and ${IMAGE}:${version}}"
