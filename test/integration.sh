#!/usr/bin/env bash
# End-to-end test. Everything runs inside an isolated docker:dind daemon so the
# developer's own containers are never scanned or recreated.
# Requires docker (+ compose plugin) and curl on the host.
set -euo pipefail

IMAGE="${IMAGE:-docker-image-updater:dev}"
DIND_IMAGE="${DIND_IMAGE:-docker:27-dind}"
DIND_PORT="${DIND_PORT:-2375}"
REG_PORT="${REG_PORT:-5005}"
REG_USER="testuser"
REG_PASS="testpass"
WEB_PORT="${WEB_PORT:-18080}"
export REF="localhost:${REG_PORT}/diu-test:latest"
WORK="$(mktemp -d)"
FAILS=0
ASSERTS=0

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
    docker logs diu-updater-noauth || true
  fi
  HOST_DOCKER rm -f -v diu-dind >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

assert_eq() { # description expected actual
  ASSERTS=$((ASSERTS + 1))
  if [ "$2" == "$3" ]; then echo "  ok   $1"; else echo "  FAIL $1: expected '$2', got '$3'"; FAILS=$((FAILS + 1)); fi
}
# Run a command quietly, printing its output only if it fails.
quiet() {
  local out
  out="$("$@" 2>&1)" || { echo "$out"; return 1; }
}

assert_match() { # description extended-regex haystack
  ASSERTS=$((ASSERTS + 1))
  if [[ "$3" =~ $2 ]]; then echo "  ok   $1"; else echo "  FAIL $1: /$2/ does not match '$3'"; FAILS=$((FAILS + 1)); fi
}

assert_contains() { # description needle haystack
  ASSERTS=$((ASSERTS + 1))
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

# The registry credentials are only ever used by the docker client running INSIDE the dind
# container (see dind_login and build_push). The host docker CLI never receives them, so nothing
# can end up in the developer's credential store, and DOCKER_CONFIG is never changed.
export REG_USER REG_PASS

echo "==> loading the updater image into dind"
env -u DOCKER_HOST docker save "$IMAGE" | docker load >/dev/null

build_push() { # version
  mkdir -p "$WORK/img"
  # Env, label and Cmd differ between versions, to check that the new image's defaults win.
  cat > "$WORK/img/Dockerfile" <<EOF
FROM busybox:1.36
ARG VERSION
ENV APP_VERSION=$1
LABEL app.version=$1
RUN echo "\$VERSION" > /version
CMD ["sh", "-c", "trap 'exit 0' TERM; : app-v$1; while :; do sleep 1; done"]
EOF
  docker build -q --build-arg VERSION="$1" -t "$REF" "$WORK/img" >/dev/null
  # Push from inside the dind container: its own client has its own config, no credential helper.
  HOST_DOCKER exec -e REF diu-dind docker push -q "$REF" >/dev/null
}

dind_login() {
  HOST_DOCKER exec -i -e REG_USER -e REG_PASS diu-dind \
    sh -c 'printf "%s" "$REG_PASS" | docker login "localhost:'"${REG_PORT}"'" -u "$REG_USER" --password-stdin' >/dev/null
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

echo "==> starting password-protected local registry"
docker run --rm --entrypoint htpasswd httpd:2.4-alpine -Bbn "$REG_USER" "$REG_PASS" > "$WORK/htpasswd"
# The dind daemon cannot mount host paths: create the container, copy the file in, then start it.
docker create --name diu-registry --label docker-image-updater.enable=false -p "${REG_PORT}:5000" \
  -e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=diu-test \
  -e REGISTRY_AUTH_HTPASSWD_PATH=/etc/docker/registry/htpasswd registry:2 >/dev/null
docker cp "$WORK/htpasswd" diu-registry:/etc/docker/registry/htpasswd
docker start diu-registry >/dev/null
for _ in $(seq 1 30); do curl -sf -m 3 -u "$REG_USER:$REG_PASS" "http://127.0.0.1:${REG_PORT}/v2/" >/dev/null && { reg_ok=1; break; }; sleep 1; done
[ "${reg_ok:-0}" == 1 ] || { echo "registry did not become ready"; exit 1; }
REG_ANON_CODE="$(curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${REG_PORT}/v2/")"
[ "$REG_ANON_CODE" == 401 ] || { echo "registry accepts anonymous requests (HTTP $REG_ANON_CODE)"; exit 1; }
dind_login
REG_ID="$(docker inspect -f '{{.Id}} {{.State.StartedAt}}' diu-registry)"

echo "==> publishing image v1 and starting workloads"
build_push 1
OLD="$(docker image inspect -f '{{.Id}}' "$REF")"
docker volume create diu-vol >/dev/null
docker run -d --name diu-run --label keep=me -e CUSTOM=yes -v diu-vol:/data "$REF" >/dev/null
docker run -d --rm --name diu-rm "$REF" >/dev/null
RM_ID="$(docker inspect -f '{{.Id}}' diu-rm)"
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

echo "==> giving the updater its registry credentials through a volume"
printf '{"auths":{"localhost:%s":{"auth":"%s"}}}\n' "$REG_PORT" "$(printf '%s:%s' "$REG_USER" "$REG_PASS" | base64 | tr -d '\n')" > "$WORK/updater-config.json"
docker volume create diu-cfg >/dev/null
docker run --rm -i -v diu-cfg:/config busybox:1.36 sh -c 'cat > /config/config.json' < "$WORK/updater-config.json"

echo "==> starting the updater"
docker run -d --name diu-updater \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v diu-cfg:/config:ro -e DOCKER_CONFIG=/config \
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
assert_eq "new image ENV takes effect" "2" "$(docker exec diu-run sh -c 'echo "$APP_VERSION"')"
assert_eq "new image label takes effect" "2" "$(docker inspect -f '{{index .Config.Labels "app.version"}}' diu-run)"
assert_contains "new image Cmd takes effect" "app-v2" "$(docker inspect -f '{{json .Config.Cmd}}' diu-run)"
assert_eq "user-set env var preserved" "yes" "$(docker exec diu-run sh -c 'echo "$CUSTOM"')"
assert_eq "--rm container still running" "true" "$(docker inspect -f '{{.State.Running}}' diu-rm)"
assert_eq "--rm container untouched (same id)" "$RM_ID" "$(docker inspect -f '{{.Id}}' diu-rm)"
assert_eq "--rm container still on the old image" "$OLD" "$(docker inspect -f '{{.Image}}' diu-rm)"
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

echo "==> forcing a scan from the web API"
SCAN_URL="http://127.0.0.1:${WEB_PORT}/api/scan"
scan_post() { curl -s -m 3 -o /dev/null -w '%{http_code}' -X POST "$@" "$SCAN_URL" || true; }
assert_eq "scan without the X-Requested-With header is rejected" "403" "$(scan_post)"
assert_eq "scan with a foreign Origin is rejected" "403" "$(scan_post -H 'X-Requested-With: docker-image-updater' -H 'Origin: http://evil.example')"
assert_eq "GET on the scan endpoint is not allowed" "405" "$(curl -s -m 3 -o /dev/null -w '%{http_code}' "$SCAN_URL" || true)"
REJECTED_STATS="$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats")"
assert_contains "rejected scan requests did not start a scan" '"scans":1,' "$REJECTED_STATS"
assert_contains "no scan is running after rejected requests" '"scanning":false' "$REJECTED_STATS"
# The last scan may have started less than 30 s ago: retry while the server answers 429.
SCAN_CODE=""
for _ in $(seq 1 40); do
  SCAN_CODE="$(scan_post -H 'X-Requested-With: docker-image-updater')"
  [ "$SCAN_CODE" == 429 ] || break
  sleep 1
done
assert_eq "scan with the header is accepted" "202" "$SCAN_CODE"
assert_match "an immediate second scan request is refused" '^(409|429)$' "$(scan_post -H 'X-Requested-With: docker-image-updater')"
scan2=0
for _ in $(seq 1 60); do
  if [[ "$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats" || true)" == *'"scans":2,'* ]]; then scan2=1; break; fi
  sleep 1
done
assert_eq "the forced scan ran (scans count reached 2)" "1" "$scan2"

assert_contains "registry credentials enabled at startup" "registry credentials enabled" "$(docker logs diu-updater 2>&1)"
assert_eq "updater logs never contain the registry password" "0" "$(docker logs diu-updater 2>&1 | grep -c -F "$REG_PASS" || true)"
REG_AUTH_B64="$(printf '%s:%s' "$REG_USER" "$REG_PASS" | base64 | tr -d '\n')"
assert_eq "updater logs never contain the base64 auth value" "0" "$(docker logs diu-updater 2>&1 | grep -c -F "$REG_AUTH_B64" || true)"
assert_eq "stats API never contains the registry password" "0" "$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats" | grep -c -F "$REG_PASS" || true)"

echo "==> negative check: an updater without credentials cannot pull from the private registry"
build_push 3
BEFORE="$(for c in diu-run diu-anonv diu-anonm "$COMPOSE_NEW" "$CUSTOM_NEW"; do docker inspect -f '{{.Id}}' "$c"; done)"
docker run -d --name diu-updater-noauth \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e CHECK_INTERVAL_MINUTES=60 "$IMAGE" >/dev/null
NOAUTH_LINE=""
for _ in $(seq 1 60); do
  NOAUTH_LINE="$(docker logs diu-updater-noauth 2>&1 | grep 'cannot check image' | grep -F "localhost:${REG_PORT}/diu-test" || true)"
  [ -n "$NOAUTH_LINE" ] && break
  sleep 1
done
assert_match "updater without credentials reports an authorization failure" 'unauthorized|no basic auth credentials' "$NOAUTH_LINE"
assert_contains "failure names the image" "localhost:${REG_PORT}/diu-test" "$NOAUTH_LINE"
AFTER="$(for c in diu-run diu-anonv diu-anonm "$COMPOSE_NEW" "$CUSTOM_NEW"; do docker inspect -f '{{.Id}}' "$c"; done)"
assert_eq "no container was changed by the updater without credentials" "$BEFORE" "$AFTER"
assert_eq "no leftover _old container after the negative check" "0" "$(docker ps -a --format '{{.Names}}' | grep -c '_old$' || true)"
assert_eq "no-credentials updater logs never contain the registry password" "0" "$(docker logs diu-updater-noauth 2>&1 | grep -c -F "$REG_PASS" || true)"
assert_eq "no-credentials updater logs never contain the base64 auth value" "0" "$(docker logs diu-updater-noauth 2>&1 | grep -c -F "$REG_AUTH_B64" || true)"
docker rm -f -v diu-updater-noauth >/dev/null

if [ "$FAILS" -ne 0 ]; then echo "$FAILS assertion(s) failed"; docker logs diu-updater; exit 1; fi
echo "PASS ($ASSERTS assertions)"
