#!/usr/bin/env bash
# End-to-end test. Everything runs inside an isolated docker:dind daemon so the
# developer's own containers are never scanned or recreated.
# Requires docker (+ compose plugin) and curl on the host.
set -euo pipefail

IMAGE="${IMAGE:-docker-image-updater:dev}"
# DIND_IMAGE selects the daemon under test: docker:27-dind uses the classic image store and
# docker:29-dind the containerd one. DIND_ARGS are extra dockerd arguments (none needed so far).
DIND_IMAGE="${DIND_IMAGE:-docker:27-dind}"
DIND_ARGS="${DIND_ARGS:-}"
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
  "$DIND_IMAGE" $DIND_ARGS >/dev/null
HOST_ID="$(HOST_DOCKER info --format '{{.ID}}')"
ready=0
for _ in $(seq 1 60); do
  if DOCKER_HOST="tcp://127.0.0.1:${DIND_PORT}" docker info >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then echo "dind did not become ready"; HOST_DOCKER logs diu-dind; exit 1; fi

# From here on every docker command targets the dind daemon.
export DOCKER_HOST="tcp://127.0.0.1:${DIND_PORT}"
DRIVER_STATUS="$(docker info --format '{{.DriverStatus}}')"
if [[ "$DRIVER_STATUS" == *io.containerd.snapshotter* ]]; then STORE=containerd; else STORE=classic; fi
echo "==> dind image $DIND_IMAGE, server $(docker version --format '{{.Server.Version}}'), image store: $STORE ($DRIVER_STATUS)"
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

# Standard HEALTHCHECK of the healthy test images: passes as soon as /version exists.
HC_OK='HEALTHCHECK --interval=1s --retries=1 CMD test -f /version'

publish_image() { # ref version [healthcheck-line] [cmd-line]
  local ref="$1" v="$2" hc="${3:-}" cmd="${4:-}"
  [ -n "$cmd" ] || cmd="CMD [\"sh\", \"-c\", \"trap 'exit 0' TERM; : app-v$v; while :; do sleep 1; done\"]"
  mkdir -p "$WORK/img"
  # Env, label and Cmd differ between versions, to check that the new image's defaults win.
  # The image reference is baked into a label so that every repository has distinct image ids.
  {
    echo "FROM busybox:1.36"
    echo "ARG VERSION"
    echo "ENV APP_VERSION=$v"
    echo "LABEL app.version=$v"
    echo "LABEL app.ref=$ref"
    echo 'RUN echo "$VERSION" > /version'
    if [ -n "$hc" ]; then echo "$hc"; fi
    echo "$cmd"
  } > "$WORK/img/Dockerfile"
  docker build -q --build-arg VERSION="$v" -t "$ref" "$WORK/img" >/dev/null
  # Push from inside the dind container: its own client has its own config, no credential helper.
  HOST_DOCKER exec -e REF="$ref" diu-dind docker push -q "$ref" >/dev/null
  # Replace the locally built image by the one pulled from the registry: with the containerd store
  # an image keeps its build origin, and a locally built image is (rightly) ignored by the updater.
  docker rmi "$ref" >/dev/null
  HOST_DOCKER exec -e REF="$ref" diu-dind docker pull -q "$ref" >/dev/null
}

# The standard image: versions above 1 carry a healthcheck, so the updated containers go through
# the "healthy" path of the post-start gate.
build_push() { # version
  if [ "$1" == 1 ]; then publish_image "$REF" "$1"; else publish_image "$REF" "$1" "$HC_OK"; fi
}

# Publishes v2 of ref while the local tag still points at v1, then moves the tag back to v1: the
# containers run v1 and the updater itself must be the one to pull v2, as in real life. The v1
# image is kept under a second name meanwhile (with the containerd store, replacing a tag deletes
# the image that had it).
publish_next_over_current() { # ref version [healthcheck-line] [cmd-line]
  local ref="$1" keep
  keep="diu-keep:$(printf '%s' "$1" | tr '/:' '__')"
  docker tag "$ref" "$keep"
  publish_image "$ref" "$2" "${3:-}" "${4:-}"
  docker tag "$keep" "$ref"
  docker rmi "$keep" >/dev/null
}
publish_v2_over_v1() { # ref [healthcheck-line] [cmd-line]
  publish_next_over_current "$1" 2 "${2:-}" "${3:-}"
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
# Locally built image: it has no registry digest and cannot be pulled, so it must be ignored silently.
mkdir -p "$WORK/local"
cat > "$WORK/local/Dockerfile" <<'EOF'
FROM busybox:1.36
CMD ["sh", "-c", "trap 'exit 0' TERM; : local-only; while :; do sleep 1; done"]
EOF
docker build -q -t local-only:1 "$WORK/local" >/dev/null
docker run -d --name diu-local local-only:1 >/dev/null
LOCAL_CID="$(docker inspect -f '{{.Id}}' diu-local)"
LOCAL_IMG="$(docker inspect -f '{{.Image}}' diu-local)"
# Policy labels. The window is 2 to 3 hours ahead of the current UTC time (the updater runs in UTC).
docker run -d --name diu-notify --label docker-image-updater.policy=notify "$REF" >/dev/null
NOTIFY_CID="$(docker inspect -f '{{.Id}}' diu-notify)"
NOW_MIN=$((10#$(date -u +%H) * 60 + 10#$(date -u +%M)))
WIN_START=$(((NOW_MIN + 120) % 1440))
WIN_END=$(((NOW_MIN + 180) % 1440))
WINDOW="$(printf '%02d:%02d-%02d:%02d' $((WIN_START / 60)) $((WIN_START % 60)) $((WIN_END / 60)) $((WIN_END % 60)))"
docker run -d --name diu-window --label "docker-image-updater.window=$WINDOW" "$REF" >/dev/null
WINDOW_CID="$(docker inspect -f '{{.Id}}' diu-window)"
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

echo "==> publishing the health gate and cleanup scenario images (v1) and starting their containers"
CRASH_REF="localhost:${REG_PORT}/diu-crash:latest"
UNHEALTHY_REF="localhost:${REG_PORT}/diu-unhealthy:latest"
CLEAN_REF="localhost:${REG_PORT}/diu-clean:latest"
PLAIN_REF="localhost:${REG_PORT}/diu-plain:latest"
for pair in "diu-crash:$CRASH_REF" "diu-unhealthy:$UNHEALTHY_REF" "diu-clean:$CLEAN_REF" "diu-plain:$PLAIN_REF"; do
  name="${pair%%:*}"; ref="${pair#*:}"
  publish_image "$ref" 1 "$HC_OK"
  docker run -d --name "$name" "$ref" >/dev/null
done
CRASH_CID="$(docker inspect -f '{{.Id}}' diu-crash)"
CRASH_OLD="$(docker inspect -f '{{.Image}}' diu-crash)"
UNHEALTHY_CID="$(docker inspect -f '{{.Id}}' diu-unhealthy)"
UNHEALTHY_OLD="$(docker inspect -f '{{.Image}}' diu-unhealthy)"
CLEAN_CID="$(docker inspect -f '{{.Id}}' diu-clean)"
CLEAN_OLD="$(docker inspect -f '{{.Image}}' diu-clean)"
PLAIN_CID="$(docker inspect -f '{{.Id}}' diu-plain)"
PLAIN_OLD="$(docker inspect -f '{{.Image}}' diu-plain)"

echo "==> an untagged image reference must pull only :latest, never every tag of the repository"
# The repository holds many tags. The container is started from the reference WITHOUT a tag, so its
# Config.Image stays untagged; a raw Engine API pull of such a reference would fetch all the tags.
UNTAGGED_REF="localhost:${REG_PORT}/diu-untagged"
for t in extra1 extra2; do
  publish_image "$UNTAGGED_REF:$t" 1 "$HC_OK"
  docker rmi "$UNTAGGED_REF:$t" >/dev/null
done
publish_image "$UNTAGGED_REF:latest" 1 "$HC_OK"
docker run -d --name diu-untagged "$UNTAGGED_REF" >/dev/null
UNTAGGED_CID="$(docker inspect -f '{{.Id}}' diu-untagged)"
UNTAGGED_OLD="$(docker inspect -f '{{.Image}}' diu-untagged)"
assert_eq "diu-untagged keeps a reference without a tag" "$UNTAGGED_REF" "$(docker inspect -f '{{.Config.Image}}' diu-untagged)"
assert_eq "only the latest tag of diu-untagged is local before the scan" "latest" "$(docker images --format '{{.Tag}}' "$UNTAGGED_REF" | sort | tr '\n' ' ' | sed 's/ $//')"

echo "==> publishing image v2"
docker tag "$REF" diu-keep:v1
build_push 2
NEW="$(docker image inspect -f '{{.Id}}' "$REF")"
[ "$OLD" != "$NEW" ] || { echo "v1 and v2 images are identical"; exit 1; }
docker tag diu-keep:v1 "$REF"
docker rmi diu-keep:v1 >/dev/null
assert_eq "the local tag points at v1 again" "$OLD" "$(docker image inspect -f '{{.Id}}' "$REF")"
# v2 of the scenario images: a crash (exit code 3, no healthcheck), a failing healthcheck, and a
# healthy one whose old image the updater is asked to clean up.
publish_v2_over_v1 "$CRASH_REF" "" 'CMD ["sh", "-c", "exit 3"]'
publish_v2_over_v1 "$UNHEALTHY_REF" 'HEALTHCHECK --interval=1s --retries=1 CMD false'
publish_v2_over_v1 "$CLEAN_REF" "$HC_OK"
# v2 without any healthcheck and with a clean long-running command: the "no healthcheck, stable
# for 10 seconds" path of the gate.
publish_v2_over_v1 "$PLAIN_REF"
publish_v2_over_v1 "$UNTAGGED_REF:latest" "$HC_OK"
assert_eq "the untagged image tag points at v1 again" "$UNTAGGED_OLD" "$(docker image inspect -f '{{.Id}}' "$UNTAGGED_REF:latest")"
assert_eq "the crash image tag points at v1 again" "$CRASH_OLD" "$(docker image inspect -f '{{.Id}}' "$CRASH_REF")"
assert_eq "the unhealthy image tag points at v1 again" "$UNHEALTHY_OLD" "$(docker image inspect -f '{{.Id}}' "$UNHEALTHY_REF")"
assert_eq "the clean image tag points at v1 again" "$CLEAN_OLD" "$(docker image inspect -f '{{.Id}}' "$CLEAN_REF")"

echo "==> giving the updater its registry credentials through a volume"
printf '{"auths":{"localhost:%s":{"auth":"%s"}}}\n' "$REG_PORT" "$(printf '%s:%s' "$REG_USER" "$REG_PASS" | base64 | tr -d '\n')" > "$WORK/updater-config.json"
docker volume create diu-cfg >/dev/null
docker run --rm -i -v diu-cfg:/config busybox:1.36 sh -c 'cat > /config/config.json' < "$WORK/updater-config.json"

echo "==> starting the updater"
# The updater image was loaded with docker load: it has no registry origin and, with the
# containerd image store, cannot be told apart from a pullable image. Exclude it explicitly.
docker run -d --name diu-updater --label docker-image-updater.enable=false \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v diu-cfg:/config:ro -e DOCKER_CONFIG=/config \
  -p "${WEB_PORT}:8080" \
  -e CHECK_INTERVAL_MINUTES=60 -e WEB_ENABLED=true \
  -e HEALTH_WAIT_SECONDS=30 -e CLEANUP_IMAGES=true "$IMAGE" >/dev/null

stat() { # name: numeric field of the stats API, whatever the JSON field order
  curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats" | grep -o "\"$1\":[0-9]*" | head -1 | cut -d: -f2
}

# The events array of a stats document (the container list has its own "name" keys, so greps on an
# event's "container" key must not see it, and vice versa).
events_of() { sed -e 's/.*"events":\[//' -e 's/\],"scanning":.*//'; }
# cstatus NAME STATS-JSON: status of container NAME in the container list.
cstatus() {
  grep -o "\"name\":\"$1\",\"image\":\"[^\"]*\",\"image_id\":\"[^\"]*\",\"status\":\"[^\"]*\"" <<< "$2" | head -1 | sed -e 's/.*"status":"//' -e 's/"$//'
}
# cchecked NAME STATS-JSON: checked_at of container NAME in the container list.
cchecked() {
  grep -o "\"name\":\"$1\",[^}]*" <<< "$2" | head -1 | sed -e 's/.*"checked_at":"//' -e 's/".*//'
}
# jnum FIELD STATS-JSON: numeric top-level field of a stats document already fetched.
jnum() {
  grep -o "\"$1\":[0-9]*" <<< "$2" | head -1 | cut -d: -f2
}
# jstr FIELD STATS-JSON: string value of a top-level field of the stats document.
jstr() {
  grep -o "\"$1\":\"[^\"]*\"" <<< "$2" | head -1 | cut -d: -f2-
}
# cdetail NAME STATS-JSON: detail of container NAME in the container list.
cdetail() {
  grep -o "\"name\":\"$1\",[^}]*" <<< "$2" | head -1 | sed -e 's/.*"detail":"//' -e 's/","policy".*//'
}

echo "==> waiting for the first scan (updates, health gate rollbacks) to finish"
ok=0
for _ in $(seq 1 240); do
  if [ "$(stat scans || true)" == 1 ]; then ok=1; break; fi
  sleep 1
done
if [ "$ok" != 1 ]; then echo "timeout waiting for the first scan"; docker logs diu-updater; exit 1; fi

echo "==> assertions"
# 5 containers of the standard image, diu-clean, diu-plain and diu-untagged are updated; diu-crash and diu-unhealthy are rolled back.
assert_eq "updates count (5 standard containers + diu-clean + diu-plain + diu-untagged)" "8" "$(stat updates)"
assert_eq "rollbacks count (diu-crash, diu-unhealthy)" "2" "$(stat rollbacks)"
assert_eq "failures count" "0" "$(stat failures)"
STATS1="$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats")"
# New containers of the standard image went through the healthy path of the gate.
assert_eq "updated container has a healthcheck that reports healthy" "healthy" "$(docker inspect -f '{{.State.Health.Status}}' diu-run)"

echo "==> health gate: a crashing new container is rolled back"
assert_eq "diu-crash is running" "true" "$(docker inspect -f '{{.State.Running}}' diu-crash)"
assert_eq "diu-crash is the original container (same id)" "$CRASH_CID" "$(docker inspect -f '{{.Id}}' diu-crash)"
assert_eq "diu-crash still runs the v1 image" "$CRASH_OLD" "$(docker inspect -f '{{.Image}}' diu-crash)"
assert_eq "diu-crash has a single container left (no leftover new one)" "1" "$(docker ps -a --filter name=diu-crash --format '{{.Names}}' | wc -l | tr -d ' ')"
assert_match "diu-crash event is rolled_back" '"container":"diu-crash","image":"[^"]*","old_id":"[^"]*","new_id":"[^"]*","status":"rolled_back","error":"new container failed health gate: [^"]*exit code 3' "$STATS1"
assert_eq "diu-crash has no updated event" "0" "$(grep -o '"container":"diu-crash","image":"[^"]*","old_id":"[^"]*","new_id":"[^"]*","status":"updated"' <<< "$STATS1" | wc -l | tr -d ' ')"

echo "==> health gate: a new container with a failing healthcheck is rolled back"
assert_eq "diu-unhealthy is running" "true" "$(docker inspect -f '{{.State.Running}}' diu-unhealthy)"
assert_eq "diu-unhealthy is the original container (same id)" "$UNHEALTHY_CID" "$(docker inspect -f '{{.Id}}' diu-unhealthy)"
assert_eq "diu-unhealthy still runs the v1 image" "$UNHEALTHY_OLD" "$(docker inspect -f '{{.Image}}' diu-unhealthy)"
assert_eq "diu-unhealthy has a single container left (no leftover new one)" "1" "$(docker ps -a --filter name=diu-unhealthy --format '{{.Names}}' | wc -l | tr -d ' ')"
assert_match "diu-unhealthy event is rolled_back" '"container":"diu-unhealthy","image":"[^"]*","old_id":"[^"]*","new_id":"[^"]*","status":"rolled_back","error":"new container failed health gate: [^"]*unhealthy' "$STATS1"

echo "==> health gate: a new container without healthcheck that stays up is kept"
PLAIN_NEW="$(docker image inspect -f '{{.Id}}' "$PLAIN_REF")"
assert_eq "diu-plain uses the new image" "$PLAIN_NEW" "$(docker inspect -f '{{.Image}}' diu-plain)"
assert_eq "diu-plain was recreated" "1" "$([ "$PLAIN_CID" != "$(docker inspect -f '{{.Id}}' diu-plain)" ] && echo 1 || echo 0)"
assert_eq "diu-plain is running" "true" "$(docker inspect -f '{{.State.Running}}' diu-plain)"
assert_eq "diu-plain has no healthcheck" "no" "$(docker inspect -f '{{if .State.Health}}yes{{else}}no{{end}}' diu-plain)"
assert_eq "diu-plain new version is running" "2" "$(docker exec diu-plain cat /version)"
assert_match "diu-plain event is updated" '"container":"diu-plain","image":"[^"]*","old_id":"[^"]*","new_id":"[^"]*","status":"updated"' "$STATS1"

echo "==> untagged reference: only :latest is pulled"
UNTAGGED_NEW="$(docker image inspect -f '{{.Id}}' "$UNTAGGED_REF:latest")"
assert_eq "diu-untagged uses the new image" "$UNTAGGED_NEW" "$(docker inspect -f '{{.Image}}' diu-untagged)"
assert_eq "diu-untagged was recreated" "1" "$([ "$UNTAGGED_CID" != "$(docker inspect -f '{{.Id}}' diu-untagged)" ] && echo 1 || echo 0)"
assert_eq "diu-untagged new version is running" "2" "$(docker exec diu-untagged cat /version)"
assert_eq "diu-untagged still has a reference without a tag" "$UNTAGGED_REF" "$(docker inspect -f '{{.Config.Image}}' diu-untagged)"
assert_eq "the extra tags of the repository were NOT pulled" "latest" "$(docker images --format '{{.Tag}}' "$UNTAGGED_REF" | sort | tr '\n' ' ' | sed 's/ $//')"
assert_eq "diu-untagged has a single updated event and no failure" "1" "$(grep -o '"container":"diu-untagged","image":"[^"]*","old_id":"[^"]*","new_id":"[^"]*","status":"updated"' <<< "$STATS1" | wc -l | tr -d ' ')"
assert_eq "no pull failure is logged for diu-untagged" "0" "$(docker logs diu-updater 2>&1 | grep -c 'diu-untagged.*\(failed\|error\)' || true)"

echo "==> cleanup of the old image"
CLEAN_NEW="$(docker image inspect -f '{{.Id}}' "$CLEAN_REF")"
assert_eq "diu-clean uses the new image" "$CLEAN_NEW" "$(docker inspect -f '{{.Image}}' diu-clean)"
assert_eq "diu-clean was recreated" "1" "$([ "$CLEAN_CID" != "$(docker inspect -f '{{.Id}}' diu-clean)" ] && echo 1 || echo 0)"
assert_eq "the old v1 image of diu-clean was removed" "gone" "$(docker image inspect "$CLEAN_OLD" >/dev/null 2>&1 && echo present || echo gone)"
UPDATER_LOGS1="$(docker logs diu-updater 2>&1)"
assert_eq "the updater logged exactly one removal of the old diu-clean image" "1" "$(grep -c 'msg="removed old image" container=diu-clean ' <<< "$UPDATER_LOGS1" || true)"
assert_eq "the shared v1 image still used by other containers was kept" "present" "$(docker image inspect "$OLD" >/dev/null 2>&1 && echo present || echo gone)"
assert_eq "the v1 images of the rolled back containers were kept" "present" "$(docker image inspect "$CRASH_OLD" "$UNHEALTHY_OLD" >/dev/null 2>&1 && echo present || echo gone)"
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
assert_eq "locally built container still running" "true" "$(docker inspect -f '{{.State.Running}}' diu-local)"
assert_eq "locally built container untouched (same id)" "$LOCAL_CID" "$(docker inspect -f '{{.Id}}' diu-local)"
assert_eq "locally built container keeps its image" "$LOCAL_IMG" "$(docker inspect -f '{{.Image}}' diu-local)"
assert_eq "updater logs show no failure for the local image" "0" "$(docker logs diu-updater 2>&1 | grep -c 'cannot check image.*local-only' || true)"
assert_eq "updater never tries to pull the local image" "0" "$(docker logs diu-updater 2>&1 | grep -c 'local-only' || true)"
assert_eq "no failure is counted" "0" "$(stat failures)"
SKIPPED="$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats" | grep -o '"skipped":[0-9]*' | cut -d: -f2)"
# Skipped: the opt-out container, the registry (opt-out) and the local container.
assert_eq "skipped count covers the opt-out and local containers" "1" "$([ "${SKIPPED:-0}" -ge 3 ] && echo 1 || echo 0)"
assert_eq "notify-policy container still running" "true" "$(docker inspect -f '{{.State.Running}}' diu-notify)"
assert_eq "notify-policy container untouched (same id)" "$NOTIFY_CID" "$(docker inspect -f '{{.Id}}' diu-notify)"
assert_eq "notify-policy container keeps the old image" "$OLD" "$(docker inspect -f '{{.Image}}' diu-notify)"
assert_contains "notify-policy update is reported as available" "diu-notify" "$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats")"
assert_eq "closed-window container still running" "true" "$(docker inspect -f '{{.State.Running}}' diu-window)"
assert_eq "closed-window container untouched (same id)" "$WINDOW_CID" "$(docker inspect -f '{{.Id}}' diu-window)"
assert_eq "closed-window container keeps the old image" "$OLD" "$(docker inspect -f '{{.Image}}' diu-window)"
assert_eq "closed-window container has no event" "0" "$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats" | events_of | grep -c 'diu-window' || true)"
assert_eq "no leftover _old container" "0" "$(docker ps -a --format '{{.Names}}' | grep -c '_old$' || true)"
assert_eq "updater still running" "true" "$(docker inspect -f '{{.State.Running}}' diu-updater)"
quiet docker compose -p diu-compose -f "$WORK/compose.yml" up -d
assert_eq "docker compose up does not recreate the container again" "$COMPOSE_NEW" "$(id_of diu-compose)"
assert_eq "stats API counts the scan" "1" "$(stat scans)"
assert_eq "healthz" "ok" "$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/healthz")"
assert_eq "healthz answers 200" "200" "$(curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${WEB_PORT}/healthz")"

echo "==> container list of the stats API"
STATS3="$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats")"
assert_contains "the stats API carries the container list" '"containers":[{' "$STATS3"
for name in diu-run "$(docker inspect -f '{{.Name}}' "$COMPOSE_NEW" | tr -d /)" "$(docker inspect -f '{{.Name}}' "$CUSTOM_NEW" | tr -d /)" \
  diu-anonv diu-anonm diu-clean diu-plain diu-untagged; do
  assert_eq "$name is listed as updated" "updated" "$(cstatus "$name" "$STATS3")"
done
assert_eq "diu-local is skipped" "skipped" "$(cstatus diu-local "$STATS3")"
assert_eq "diu-local is skipped because it was built locally" "locally built image" "$(cdetail diu-local "$STATS3")"
assert_eq "diu-optout is skipped" "skipped" "$(cstatus diu-optout "$STATS3")"
assert_eq "diu-optout is skipped because of the label" "opted out (enable=false)" "$(cdetail diu-optout "$STATS3")"
assert_eq "the registry is skipped" "skipped" "$(cstatus diu-registry "$STATS3")"
assert_eq "diu-notify is update-available" "update-available" "$(cstatus diu-notify "$STATS3")"
assert_eq "diu-rm (AutoRemove) is update-available" "update-available" "$(cstatus diu-rm "$STATS3")"
assert_eq "diu-window is waiting-window" "waiting-window" "$(cstatus diu-window "$STATS3")"
assert_eq "diu-crash is rolled-back" "rolled-back" "$(cstatus diu-crash "$STATS3")"
assert_eq "diu-unhealthy is rolled-back" "rolled-back" "$(cstatus diu-unhealthy "$STATS3")"
assert_contains "the rollback detail names the health gate" "health gate failed:" "$(cdetail diu-crash "$STATS3")"

echo "==> Prometheus metrics"
METRICS_HEADERS="$(curl -s -m 3 -D - -o /dev/null "http://127.0.0.1:${WEB_PORT}/metrics" | tr -d '\r')"
METRICS="$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/metrics")"
assert_contains "metrics content type" 'text/plain; version=0.0.4; charset=utf-8' "$METRICS_HEADERS"
assert_contains "diu_updates_total" $'\ndiu_updates_total 8\n' $'\n'"$METRICS"
assert_contains "diu_rollbacks_total" $'\ndiu_rollbacks_total 2\n' $'\n'"$METRICS"
assert_contains "diu_failures_total" $'\ndiu_failures_total 0\n' $'\n'"$METRICS"
assert_contains "diu_scans_total" $'\ndiu_scans_total 1\n' $'\n'"$METRICS"
assert_contains "diu_build_info" $'\ndiu_build_info{' $'\n'"$METRICS"
assert_contains "scan counter is typed" '# TYPE diu_scans_total counter' "$METRICS"
assert_contains "containers by status" 'diu_containers{status="rolled-back"} 2' "$METRICS"
assert_contains "containers waiting for a window" 'diu_containers{status="waiting-window"} 1' "$METRICS"
assert_eq "metrics never contain the registry password" "0" "$(grep -c -F "$REG_PASS" <<< "$METRICS" || true)"
assert_eq "POST on /metrics is not allowed" "405" "$(curl -s -m 3 -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${WEB_PORT}/metrics" || true)"

echo "==> forcing a scan from the web API"
SCAN_URL="http://127.0.0.1:${WEB_PORT}/api/scan"
scan_post() { curl -s -m 3 -o /dev/null -w '%{http_code}' -X POST "$@" "$SCAN_URL" || true; }
assert_eq "scan without the X-Requested-With header is rejected" "403" "$(scan_post)"
assert_eq "scan with a foreign Origin is rejected" "403" "$(scan_post -H 'X-Requested-With: docker-image-updater' -H 'Origin: http://evil.example')"
assert_eq "GET on the scan endpoint is not allowed" "405" "$(curl -s -m 3 -o /dev/null -w '%{http_code}' "$SCAN_URL" || true)"
REJECTED_STATS="$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats")"
assert_eq "rejected scan requests did not start a scan" "1" "$(stat scans)"
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
  if [ "$(stat scans || true)" == 2 ]; then scan2=1; break; fi
  sleep 1
done
assert_eq "the forced scan ran (scans count reached 2)" "1" "$scan2"
STATS2="$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats")"
assert_eq "no failure after the forced scan" "0" "$(stat failures)"
for pair in "diu-local:$LOCAL_CID:$LOCAL_IMG" "diu-notify:$NOTIFY_CID:$OLD" "diu-window:$WINDOW_CID:$OLD"; do
  IFS=: read -r name cid img <<< "$pair"
  assert_eq "$name keeps its container id after the forced scan" "$cid" "$(docker inspect -f '{{.Id}}' "$name")"
  assert_eq "$name keeps its image after the forced scan" "$img" "$(docker inspect -f '{{.Image}}' "$name")"
done
# A defective image is not retried: the forced scan neither recreates nor rolls back again.
assert_eq "rollbacks stay at 2 after the forced scan" "2" "$(stat rollbacks)"
assert_eq "updates stay at 8 after the forced scan" "8" "$(stat updates)"
assert_eq "diu-crash keeps its container id after the forced scan" "$CRASH_CID" "$(docker inspect -f '{{.Id}}' diu-crash)"
assert_eq "diu-crash still runs v1 after the forced scan" "$CRASH_OLD" "$(docker inspect -f '{{.Image}}' diu-crash)"
assert_eq "diu-unhealthy keeps its container id after the forced scan" "$UNHEALTHY_CID" "$(docker inspect -f '{{.Id}}' diu-unhealthy)"
assert_eq "diu-unhealthy still runs v1 after the forced scan" "$UNHEALTHY_OLD" "$(docker inspect -f '{{.Image}}' diu-unhealthy)"
assert_eq "the updater skipped both known bad images" "2" "$(docker logs diu-updater 2>&1 | grep -c 'skipping image that previously failed the health gate' || true)"
# The first scan may start before the daemon lists the updater's own container as running: check it
# on the second scan.
assert_eq "the updater itself is listed as skipped (opt-out) after the forced scan" "skipped" "$(cstatus diu-updater "$STATS2")"
assert_eq "diu-notify has exactly one event" "1" "$(grep -o '"container":"diu-notify"' <<< "$STATS2" | wc -l | tr -d ' ')"
assert_contains "the notify event has the available status" '"status":"available"' "$STATS2"
assert_eq "diu-window has no event after the forced scan" "0" "$(events_of <<< "$STATS2" | grep -c 'diu-window' || true)"

echo "==> scanning a single container from the web API"
# Publish v3 of two images used by two different, already updated containers (they run v2).
CLEAN_V2="$(docker inspect -f '{{.Image}}' diu-clean)"
CLEAN_CID2="$(docker inspect -f '{{.Id}}' diu-clean)"
PLAIN_V2="$(docker inspect -f '{{.Image}}' diu-plain)"
PLAIN_CID2="$(docker inspect -f '{{.Id}}' diu-plain)"
publish_next_over_current "$CLEAN_REF" 3 "$HC_OK"
publish_next_over_current "$PLAIN_REF" 3
assert_eq "the clean image tag points at v2 again" "$CLEAN_V2" "$(docker image inspect -f '{{.Id}}' "$CLEAN_REF")"
assert_eq "the plain image tag points at v2 again" "$PLAIN_V2" "$(docker image inspect -f '{{.Id}}' "$PLAIN_REF")"
# Wait for the end of the forced scan (the scan counter moves before the scanning flag drops).
idle=0
for _ in $(seq 1 60); do
  if grep -q '"scanning":false' <<< "$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats")"; then idle=1; break; fi
  sleep 1
done
assert_eq "no scan is running before the targeted scan" "1" "$idle"
STATS_PRE="$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats")"
assert_eq "scan count before the targeted scan" "2" "$(stat scans)"
# scan_post_body ARGS...: prints "<http code> <body>".
scan_post_body() { curl -s -m 3 -w ' %{http_code}' -X POST "$@" || true; }
assert_match "a targeted scan of an unknown container is a 404" '^\{"error":"[^"]*"\} 404$' "$(scan_post_body -H 'X-Requested-With: docker-image-updater' "$SCAN_URL?container=diu-nope" | tr -d '\n')"
assert_match "a targeted scan of a malformed name is a 400" '^\{"error":"[^"]*"\} 400$' "$(scan_post_body -H 'X-Requested-With: docker-image-updater' "$SCAN_URL?container=a%2Fb" | tr -d '\n')"
assert_match "a targeted scan with an empty name is a 400" ' 400$' "$(scan_post_body -H 'X-Requested-With: docker-image-updater' "$SCAN_URL?container=" | tr -d '\n')"
assert_match "a targeted scan without the header is a 403" ' 403$' "$(scan_post_body "$SCAN_URL?container=diu-clean" | tr -d '\n')"
assert_match "a targeted scan with a foreign Origin is a 403" ' 403$' "$(scan_post_body -H 'X-Requested-With: docker-image-updater' -H 'Origin: http://evil.example' "$SCAN_URL?container=diu-clean" | tr -d '\n')"
assert_eq "rejected targeted requests did not start a scan" "2" "$(stat scans)"
# The previous scan may have started less than 5 s ago: retry while the server answers 429.
TARGET_OUT=""
for _ in $(seq 1 20); do
  TARGET_OUT="$(scan_post_body -H 'X-Requested-With: docker-image-updater' "$SCAN_URL?container=diu-clean" | tr -d '\n')"
  [[ "$TARGET_OUT" == *' 429' ]] || break
  sleep 1
done
assert_eq "the targeted scan of diu-clean is accepted and names the container" '{"status":"started","container":"diu-clean"} 202' "$TARGET_OUT"
assert_match "a second request right after is refused" '^(409|429)$' "$(scan_post -H 'X-Requested-With: docker-image-updater')"
scan3=0
for _ in $(seq 1 120); do
  if [ "$(stat scans || true)" == 3 ] && grep -q '"scanning":false' <<< "$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats")"; then scan3=1; break; fi
  sleep 1
done
assert_eq "the targeted scan ran (scans count reached 3)" "1" "$scan3"
STATS4="$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats")"
CLEAN_V3="$(docker image inspect -f '{{.Id}}' "$CLEAN_REF")"
assert_eq "the updater pulled the new diu-clean image" "1" "$([ "$CLEAN_V3" != "$CLEAN_V2" ] && echo 1 || echo 0)"
assert_eq "diu-clean runs the v3 image" "$CLEAN_V3" "$(docker inspect -f '{{.Image}}' diu-clean)"
assert_eq "diu-clean was recreated by the targeted scan" "1" "$([ "$CLEAN_CID2" != "$(docker inspect -f '{{.Id}}' diu-clean)" ] && echo 1 || echo 0)"
assert_eq "diu-clean new version is running" "3" "$(docker exec diu-clean cat /version)"
assert_eq "diu-clean has an updated event for each of its two updates" "2" "$(events_of <<< "$STATS4" | grep -o '"container":"diu-clean","image":"[^"]*","old_id":"[^"]*","new_id":"[^"]*","status":"updated"' | wc -l | tr -d ' ')"
assert_eq "diu-plain was NOT updated by the targeted scan (same container id)" "$PLAIN_CID2" "$(docker inspect -f '{{.Id}}' diu-plain)"
assert_eq "diu-plain still runs v2" "$PLAIN_V2" "$(docker inspect -f '{{.Image}}' diu-plain)"
assert_eq "diu-plain image was NOT pulled by the targeted scan (tag still on v2)" "$PLAIN_V2" "$(docker image inspect -f '{{.Id}}' "$PLAIN_REF")"
assert_eq "updates count after the targeted scan" "9" "$(stat updates)"
assert_eq "rollbacks count after the targeted scan" "2" "$(stat rollbacks)"
assert_eq "failures count after the targeted scan" "0" "$(stat failures)"
assert_eq "the targeted scan counts as a scan" "3" "$(stat scans)"
assert_eq "the watched gauge is not overwritten by the partial scan" "$(jnum watched "$STATS_PRE")" "$(jnum watched "$STATS4")"
assert_eq "the skipped gauge is not overwritten by the partial scan" "$(jnum skipped "$STATS_PRE")" "$(jnum skipped "$STATS4")"
assert_eq "the targeted scan does not move the periodic schedule" "$(jstr next_scan "$STATS_PRE")" "$(jstr next_scan "$STATS4")"
assert_eq "the container list still has the same number of entries" "$(grep -o '"checked_at"' <<< "$STATS_PRE" | wc -l | tr -d ' ')" "$(grep -o '"checked_at"' <<< "$STATS4" | wc -l | tr -d ' ')"
assert_eq "diu-clean is listed as updated" "updated" "$(cstatus diu-clean "$STATS4")"
assert_eq "diu-clean entry was refreshed" "1" "$([ "$(cchecked diu-clean "$STATS_PRE")" != "$(cchecked diu-clean "$STATS4")" ] && echo 1 || echo 0)"
for name in diu-plain diu-notify diu-window diu-crash diu-local; do
  assert_eq "$name entry is untouched by the targeted scan (same checked_at)" "$(cchecked $name "$STATS_PRE")" "$(cchecked $name "$STATS4")"
done
assert_eq "diu-plain is still listed as before" "$(cstatus diu-plain "$STATS_PRE")" "$(cstatus diu-plain "$STATS4")"
assert_eq "no leftover _old container after the targeted scan" "0" "$(docker ps -a --format '{{.Names}}' | grep -c '_old$' || true)"

echo "==> a full forced scan after the targeted one updates the other container"
# The 30 s spacing of a full forced scan is measured from the start of the targeted scan.
SCAN_CODE=""
for _ in $(seq 1 45); do
  SCAN_CODE="$(scan_post -H 'X-Requested-With: docker-image-updater')"
  [ "$SCAN_CODE" == 429 ] || break
  sleep 1
done
assert_eq "the full forced scan is accepted once the spacing is over" "202" "$SCAN_CODE"
scan4=0
for _ in $(seq 1 150); do
  if [ "$(stat scans || true)" == 4 ] && grep -q '"scanning":false' <<< "$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats")"; then scan4=1; break; fi
  sleep 1
done
assert_eq "the full forced scan ran (scans count reached 4)" "1" "$scan4"
PLAIN_V3="$(docker image inspect -f '{{.Id}}' "$PLAIN_REF")"
assert_eq "the full scan pulled the new diu-plain image" "1" "$([ "$PLAIN_V3" != "$PLAIN_V2" ] && echo 1 || echo 0)"
assert_eq "diu-plain now runs the v3 image" "$PLAIN_V3" "$(docker inspect -f '{{.Image}}' diu-plain)"
assert_eq "diu-plain was recreated by the full scan" "1" "$([ "$PLAIN_CID2" != "$(docker inspect -f '{{.Id}}' diu-plain)" ] && echo 1 || echo 0)"
assert_eq "diu-clean keeps the container updated by the targeted scan" "$CLEAN_V3" "$(docker inspect -f '{{.Image}}' diu-clean)"
assert_eq "updates count after the full scan" "10" "$(stat updates)"
assert_eq "rollbacks count after the full scan" "2" "$(stat rollbacks)"
assert_eq "failures count after the full scan" "0" "$(stat failures)"
assert_eq "no leftover _old container after the full scan" "0" "$(docker ps -a --format '{{.Names}}' | grep -c '_old$' || true)"

assert_contains "registry credentials enabled at startup" "registry credentials enabled" "$(docker logs diu-updater 2>&1)"
assert_eq "updater logs never contain the registry password" "0" "$(docker logs diu-updater 2>&1 | grep -c -F "$REG_PASS" || true)"
REG_AUTH_B64="$(printf '%s:%s' "$REG_USER" "$REG_PASS" | base64 | tr -d '\n')"
assert_eq "updater logs never contain the base64 auth value" "0" "$(docker logs diu-updater 2>&1 | grep -c -F "$REG_AUTH_B64" || true)"
assert_eq "stats API never contains the registry password" "0" "$(curl -sf -m 3 "http://127.0.0.1:${WEB_PORT}/api/stats" | grep -c -F "$REG_PASS" || true)"

echo "==> the image HEALTHCHECK (-healthcheck against the built-in web server)"
# The updater container runs with the web server enabled: the HEALTHCHECK baked into the image
# must turn it healthy (first probe one interval, 60 s, after the start). scratch has no shell, so
# the status is read from the daemon instead of running the probe by hand.
HC_STATUS=""
for _ in $(seq 1 150); do
  HC_STATUS="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' diu-updater)"
  [ "$HC_STATUS" == healthy ] && break
  sleep 1
done
assert_eq "the updater container becomes healthy through the image HEALTHCHECK" "healthy" "$HC_STATUS"

echo "==> negative check: an updater without credentials cannot pull from the private registry"
build_push 3
BEFORE="$(for c in diu-run diu-anonv diu-anonm "$COMPOSE_NEW" "$CUSTOM_NEW"; do docker inspect -f '{{.Id}}' "$c"; done)"
docker run -d --name diu-updater-noauth --label docker-image-updater.enable=false \
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
