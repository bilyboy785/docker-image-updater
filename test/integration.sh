#!/usr/bin/env bash
# End-to-end test. Everything runs inside an isolated docker:dind daemon so the
# developer's own containers are never scanned or recreated.
# Requires docker (+ compose plugin) and curl on the host.
set -euo pipefail

IMAGE="${IMAGE:-docker-image-updater:dev}"
DIND_IMAGE="${DIND_IMAGE:-docker:27-dind}"
DIND_PORT="${DIND_PORT:-2375}"
REG_PORT="${REG_PORT:-5005}"
WEB_PORT="${WEB_PORT:-18080}"
export REF="localhost:${REG_PORT}/diu-test:latest"
WORK="$(mktemp -d)"
FAILS=0

HOST_DOCKER() { env -u DOCKER_HOST docker "$@"; }

DIND_VERIFIED=0

cleanup() {
  local rc=$?
  set +e
  # On failure, print diagnostics from the dind daemon (never the host one).
  if [ "$rc" -ne 0 ] && [ "$DIND_VERIFIED" == 1 ]; then
    echo "==> FAILURE (exit $rc), diagnostics from the dind daemon"
    docker ps -a || true
    docker logs diu-updater || true
  fi
  HOST_DOCKER rm -f -v diu-dind >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

assert_eq() { # description expected actual
  if [ "$2" == "$3" ]; then echo "  ok   $1"; else echo "  FAIL $1: expected '$2', got '$3'"; FAILS=$((FAILS + 1)); fi
}
# Run a command quietly, printing its output only if it fails.
quiet() {
  local out
  out="$("$@" 2>&1)" || { echo "$out"; return 1; }
}

assert_contains() { # description needle haystack
  if [[ "$3" == *"$2"* ]]; then echo "  ok   $1"; else echo "  FAIL $1: '$2' not found in '$3'"; FAILS=$((FAILS + 1)); fi
}

echo "==> starting isolated docker:dind daemon"
HOST_DOCKER rm -f -v diu-dind >/dev/null 2>&1 || true
HOST_DOCKER run -d --privileged --name diu-dind -e DOCKER_TLS_CERTDIR= \
  -p "127.0.0.1:${DIND_PORT}:2375" -p "127.0.0.1:${REG_PORT}:${REG_PORT}" -p "127.0.0.1:${WEB_PORT}:${WEB_PORT}" \
  "$DIND_IMAGE" >/dev/null
HOST_ID="$(HOST_DOCKER info --format '{{.ID}}')"
ready=0
for _ in $(seq 1 60); do
  if DOCKER_HOST="tcp://127.0.0.1:${DIND_PORT}" docker info >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then echo "dind did not become ready"; HOST_DOCKER logs diu-dind; exit 1; fi

# From here on every docker command targets the dind daemon.
export DOCKER_HOST="tcp://127.0.0.1:${DIND_PORT}"
DIND_ID="$(docker info --format '{{.ID}}')"
if [ -z "$DIND_ID" ] || [ "$DIND_ID" == "$HOST_ID" ] || docker ps -a --format '{{.Names}}' | grep -q '^diu-dind$'; then
  echo "SAFETY: DOCKER_HOST does not point at the isolated dind daemon, aborting"; exit 1
fi
DIND_VERIFIED=1

echo "==> loading the updater image into dind"
env -u DOCKER_HOST docker save "$IMAGE" | docker load >/dev/null

build_push() { # version
  mkdir -p "$WORK/img"
  cat > "$WORK/img/Dockerfile" <<'EOF'
FROM busybox:1.36
ARG VERSION
RUN echo "$VERSION" > /version
CMD ["sh", "-c", "trap 'exit 0' TERM; while :; do sleep 1; done"]
EOF
  docker build -q --build-arg VERSION="$1" -t "$REF" "$WORK/img" >/dev/null
  docker push -q "$REF" >/dev/null
}

cat > "$WORK/compose.yml" <<'EOF'
services:
  web:
    image: ${REF}
    labels:
      keep: "me"
    volumes:
      - data:/data
    networks: [backend]
volumes:
  data:
networks:
  backend:
EOF

echo "==> starting local registry"
docker run -d --name diu-registry --label docker-image-updater.enable=false -p "${REG_PORT}:5000" registry:2 >/dev/null
for _ in $(seq 1 30); do curl -sf -m 3 "http://127.0.0.1:${REG_PORT}/v2/" >/dev/null && { reg_ok=1; break; }; sleep 1; done
[ "${reg_ok:-0}" == 1 ] || { echo "registry did not become ready"; exit 1; }
REG_ID="$(docker inspect -f '{{.Id}} {{.State.StartedAt}}' diu-registry)"

echo "==> publishing image v1 and starting workloads"
build_push 1
OLD="$(docker image inspect -f '{{.Id}}' "$REF")"
docker volume create diu-vol >/dev/null
docker run -d --name diu-run --label keep=me -v diu-vol:/data "$REF" >/dev/null
docker run -d --name diu-optout --label docker-image-updater.enable=false "$REF" >/dev/null
docker run -d --name diu-anonv -v /anonv "$REF" >/dev/null
docker run -d --name diu-anonm --mount type=volume,dst=/anonm "$REF" >/dev/null
quiet docker compose -p diu-compose -f "$WORK/compose.yml" up -d
quiet docker compose -p diu-custom -f "$WORK/compose.yml" up -d

id_of() { docker ps -q --filter "label=com.docker.compose.project=$1"; }
COMPOSE_ID="$(id_of diu-compose)"
CUSTOM_ID="$(id_of diu-custom)"
docker exec diu-run sh -c 'echo persisted > /data/marker'
docker exec "$COMPOSE_ID" sh -c 'echo persisted > /data/marker'
docker exec "$CUSTOM_ID" sh -c 'echo persisted > /data/marker'
docker exec diu-anonv sh -c 'echo persisted > /anonv/marker'
docker exec diu-anonm sh -c 'echo persisted > /anonm/marker'

echo "==> publishing image v2"
build_push 2
NEW="$(docker image inspect -f '{{.Id}}' "$REF")"
[ "$OLD" != "$NEW" ] || { echo "v1 and v2 images are identical"; exit 1; }

echo "==> starting the updater"
docker run -d --name diu-updater \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -p "${WEB_PORT}:8080" \
  -e CHECK_INTERVAL_MINUTES=60 -e WEB_ENABLED=true "$IMAGE" >/dev/null

echo "==> waiting for the updates to be applied"
ok=0
for _ in $(seq 1 120); do
  stats="$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats" || true)"
  if [[ "$stats" == *'"updates":5,'* && "$stats" == *'"scans":1,'* ]]; then ok=1; break; fi
  sleep 1
done
if [ "$ok" != 1 ]; then echo "timeout waiting for updates"; docker logs diu-updater; exit 1; fi

echo "==> assertions"
COMPOSE_NEW="$(id_of diu-compose)"
CUSTOM_NEW="$(id_of diu-custom)"
assert_eq "docker run container uses the new image" "$NEW" "$(docker inspect -f '{{.Image}}' diu-run)"
assert_eq "compose container uses the new image" "$NEW" "$(docker inspect -f '{{.Image}}' "$COMPOSE_NEW")"
assert_eq "compose -p container uses the new image" "$NEW" "$(docker inspect -f '{{.Image}}' "$CUSTOM_NEW")"
assert_eq "anonymous -v volume container uses the new image" "$NEW" "$(docker inspect -f '{{.Image}}' diu-anonv)"
assert_eq "anonymous --mount volume container uses the new image" "$NEW" "$(docker inspect -f '{{.Image}}' diu-anonm)"
assert_eq "opt-out container untouched" "$OLD" "$(docker inspect -f '{{.Image}}' diu-optout)"
assert_eq "registry untouched" "$REG_ID" "$(docker inspect -f '{{.Id}} {{.State.StartedAt}}' diu-registry)"
assert_eq "new version is running" "2" "$(docker exec diu-run cat /version)"
assert_eq "custom label preserved" "me" "$(docker inspect -f '{{index .Config.Labels "keep"}}' diu-run)"
assert_eq "compose project label preserved" "diu-compose" "$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$COMPOSE_NEW")"
assert_eq "compose -p project label preserved" "diu-custom" "$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$CUSTOM_NEW")"
assert_eq "named volume data survives" "persisted" "$(docker exec diu-run cat /data/marker)"
assert_eq "compose volume data survives" "persisted" "$(docker exec "$COMPOSE_NEW" cat /data/marker)"
assert_eq "compose -p volume data survives" "persisted" "$(docker exec "$CUSTOM_NEW" cat /data/marker)"
assert_eq "anonymous -v volume data survives" "persisted" "$(docker exec diu-anonv cat /anonv/marker)"
assert_eq "anonymous --mount volume data survives" "persisted" "$(docker exec diu-anonm cat /anonm/marker)"
assert_contains "compose network preserved" "diu-compose_backend" "$(docker inspect -f '{{json .NetworkSettings.Networks}}' "$COMPOSE_NEW")"
assert_contains "compose -p network preserved" "diu-custom_backend" "$(docker inspect -f '{{json .NetworkSettings.Networks}}' "$CUSTOM_NEW")"
assert_eq "no leftover _old container" "0" "$(docker ps -a --format '{{.Names}}' | grep -c '_old$' || true)"
assert_eq "updater still running" "true" "$(docker inspect -f '{{.State.Running}}' diu-updater)"
quiet docker compose -p diu-compose -f "$WORK/compose.yml" up -d
assert_eq "docker compose up does not recreate the container again" "$COMPOSE_NEW" "$(id_of diu-compose)"
assert_contains "stats API counts the scan" '"scans":1,' "$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats")"
assert_eq "healthz" "ok" "$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/healthz")"

if [ "$FAILS" -ne 0 ]; then echo "$FAILS assertion(s) failed"; docker logs diu-updater; exit 1; fi
echo "PASS"
