# docker-image-updater

Keeps your running Docker containers up to date: it pulls fresh images on a schedule and recreates the containers that changed.

[![GitHub Actions Workflow Status](https://img.shields.io/github/actions/workflow/status/bilyboy785/docker-image-updater/ci.yml)](https://github.com/bilyboy785/docker-image-updater/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/bilyboy785/docker-image-updater)](https://github.com/bilyboy785/docker-image-updater/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Image](https://img.shields.io/badge/ghcr.io-docker--image--updater-informational)](https://github.com/bilyboy785/docker-image-updater/pkgs/container/docker-image-updater)
[![Image size](https://ghcr-badge.egpl.dev/bilyboy785/docker-image-updater/size)](https://github.com/bilyboy785/docker-image-updater/pkgs/container/docker-image-updater)

## Features

- Automatic updates for every running container.
- Works with containers started by `docker run`, `docker compose` and `docker compose -p`.
- Periodic check, with the interval set in minutes.
- Private registry support (GHCR, Docker Hub, self-hosted) through a mounted Docker config file.
- Telegram notifications (optional).
- Optional statistics dashboard, with a button to force a scan.
- Per-container opt-out label, update policy (`auto` or `notify`) and update time windows.
- Locally built images are ignored silently.
- Dry-run mode: only notify, never recreate.
- Post-start health gate: the update is only kept once the new container proves it works, otherwise it is rolled back.
- Automatic rollback when an update fails.
- Optional cleanup of the previous image after an update.
- Tiny `scratch` image (about 3 MB), multi-arch (`linux/amd64`, `linux/arm64`).
- No third-party Go dependencies.

## Quick start

With `docker run`:

```bash
docker run -d --name docker-image-updater --restart unless-stopped --stop-timeout 60 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v updater-data:/data \
  -e CHECK_INTERVAL_MINUTES=60 \
  -e STATE_FILE=/data/state.json \
  ghcr.io/bilyboy785/docker-image-updater:latest
```

With Docker Compose:

```yaml
services:
  docker-image-updater:
    image: ghcr.io/bilyboy785/docker-image-updater:latest
    restart: unless-stopped
    stop_grace_period: 60s   # the update in progress (a rollback takes up to about 60 s) and the notification flush must fit (docker stop waits 10 s by default)
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - updater-data:/data
      # - /path/to/dockerconfig:/config:ro   # optional, see "Private registries"
    environment:
      CHECK_INTERVAL_MINUTES: "60"
      TELEGRAM_BOT_TOKEN: "123456:ABC-your-bot-token"
      TELEGRAM_CHAT_ID: "123456789"
      WEB_ENABLED: "true"
      STATE_FILE: /data/state.json
      # DOCKER_CONFIG: /config   # optional, see "Private registries"
    ports:
      - "127.0.0.1:8080:8080"
volumes:
  updater-data:
```

## Configuration

Every setting can be given as an environment variable or as a command-line flag. A flag overrides the environment variable. Run with `-h` (or `-help`) to print the flags, their environment variables and defaults.

| Environment variable     | Flag               | Default                 | Description |
|--------------------------|--------------------|-------------------------|-------------|
| `CHECK_INTERVAL_MINUTES` | `-interval`        | `60`                    | Time between two scans, in minutes (integer from 1 to 525600, one year). |
| `TELEGRAM_BOT_TOKEN`     | `-telegram-token`  | empty                   | Telegram bot token. Notifications are disabled when empty. |
| `TELEGRAM_CHAT_ID`       | `-telegram-chat`   | empty                   | Telegram chat ID. Must be set together with the token. |
| `DRY_RUN`                | `-dry-run`         | `false`                 | Only notify about available updates, never recreate containers. |
| `WEB_ENABLED`            | `-web`             | `false`                 | Enable the statistics web server. |
| `WEB_ADDR`               | `-web-addr`        | `:8080`                 | Listen address of the web server. |
| `STATE_FILE`             | `-state-file`      | empty                   | File used to persist statistics across restarts. Statistics are not persisted when empty. |
| `DOCKER_SOCKET`          | `-socket`          | `/var/run/docker.sock`  | Path of the Docker socket. |
| `HEALTH_WAIT_SECONDS`    | `-health-wait`     | `60`                    | How long a new container is watched before the update is accepted, in seconds (integer from 0 to 600). `0` disables the health gate. See [Health gate](#health-gate). |
| `CLEANUP_IMAGES`         | `-cleanup-images`  | `false`                 | Remove the previous image after a successful update. See [Cleaning up old images](#cleaning-up-old-images). |
| `TZ`                     | none               | `UTC`                   | Time zone used to evaluate update windows, for example `Europe/Paris`. The zone database is embedded in the binary. See [Update policy and windows](#update-policy-and-windows). |
| `DOCKER_CONFIG`          | `-docker-config`   | empty                   | Directory containing a Docker `config.json` with registry credentials. Pulls are anonymous when empty. See [Private registries](#private-registries). |

## How it works

On each scan the updater lists the running containers and, for every distinct image reference, pulls the image and compares the resulting image ID with the one each container is running. A container whose image ID differs is updated.

Containers that are skipped:

- containers whose image is pinned by digest (`image@sha256:...`);
- containers opted out with the `docker-image-updater.enable=false` label (see below);
- containers running a locally built image (compose `build:`, `docker build -t name`, `docker load`), which cannot be pulled and would only produce failed pulls. Such a group is ignored silently (no pull, no event, no notification) and counted as skipped in the statistics. See [Locally built images](#locally-built-images) for how they are recognised.

The updater itself (identified by its hostname or by the `docker-image-updater.self=true` label) is watched and notified about, but it is never recreated: update it manually. Containers started with `--rm` (AutoRemove) are handled the same way, see below.

To update a container, the updater inspects it and recreates it from that description:

1. rename the old container to `<name>_old`;
2. stop it;
3. create a new container with the original name and configuration, running the new image;
4. connect the additional networks;
5. start the new container;
6. watch the new container (see [Health gate](#health-gate));
7. remove the old container, only once the new one passed that check.

If a step fails after the old container has been renamed and stopped (steps 2 to 6), the new container (if any) is removed, the old one is renamed back and started again, and the failure is reported as a rollback. A failure before that point (for example when the container cannot be inspected or renamed) leaves the old container untouched and is reported as failed. If the rollback itself fails, the error says that the service may be down and names the stopped old container (`<name>_old`, or `<name>` if only the restart failed) so you can start it by hand.

Because the new container is rebuilt from `docker inspect`, this works the same for `docker run`, `docker compose` and `docker compose -p` containers:

- all labels are preserved, including the `com.docker.compose.*` ones, and the `com.docker.compose.image` label is refreshed to the new image ID so that a later `docker compose up` does not recreate the container again;
- anonymous volumes are re-bound to the new container, so their data is kept;
- networks, aliases and static IP settings are carried over;
- with `network_mode: container:X`, the hostname is not set on the new container, as Docker forbids it;
- the image configuration is not inherited from the old image: `docker inspect` merges the image defaults with the container settings, so the updater drops the values that merely repeat the old image's defaults (environment variables, labels, command, entrypoint, working directory, user, stop signal, healthcheck, exposed ports and volumes). The defaults of the new image take effect, while everything you set yourself (your own variables, labels, command and so on) is kept. If the old image is already gone, the inspected configuration is used as is.

Other behaviours:

- In dry-run mode, for the updater itself and for containers started with `--rm`, an "update available" notification is sent once per container and new image, until the updater restarts.
- Containers started with `--rm` (AutoRemove) are never touched: stopping them would make Docker delete them, so no rollback would be possible. The updater only notifies you, and you must update them manually.
- The configuration of the image a container currently runs is read before the new image is pulled, because some image stores may remove or untag the previous image once the tag moves.
- If pulling an image fails (for example a private image or a registry outage), the failure is logged, counted and notified once per image and error, not on every scan.
- When the updated container is the network provider of other containers (`network_mode: container:X`), the success notification names those containers: they share its network stack and must be restarted or recreated manually.
- Docker calls have timeouts (1 minute, and 30 minutes for pulls), so a stuck daemon cannot block the scan loop forever.

### Health gate

Starting is not enough: an application can crash a few seconds after it starts, and by then the old container would already be gone. After the new container has started, the updater keeps the old (stopped) container and watches the new one for up to `HEALTH_WAIT_SECONDS` (60 seconds by default, from 0 to 600). The old container is removed only when the new one passes. If it fails, the update is rolled back as described above (the new container is removed, the old one is renamed back and restarted), the event is recorded with the `rolled_back` status, and the notification reads "rolled back to the previous image" followed by the reason.

What counts as healthy depends on the image:

- **The container has a healthcheck** (`HEALTHCHECK` in the image or `healthcheck:` in compose): the updater follows the healthcheck. `healthy` passes at once. `unhealthy` fails. `starting` keeps waiting. A container that exits or restarts while its healthcheck is still starting fails.
- **No healthcheck**: the container must be running, not restarting, and its restart count must not increase for 10 seconds in a row (or for `HEALTH_WAIT_SECONDS` if that is shorter). A container that has stopped passes when its exit code is `0` (a one-shot job that finished gracefully) and fails with any other exit code. A restarting container, or a restart count that goes up (for example under `restart: always`), fails.
- If the container vanishes (the daemon answers 404) the check fails. Other errors while inspecting it are tolerated for up to 3 consecutive attempts, then the check fails.

Slow starters: when the healthcheck is still `starting` after `HEALTH_WAIT_SECONDS`, the update is kept, because an application with a long `start_period` must not be rolled back wrongly. The success notification then says "healthcheck still starting after Ns, update kept; raise HEALTH_WAIT_SECONDS for slow starters". Raise the value if you want the updater to wait for such an application.

Set `HEALTH_WAIT_SECONDS=0` to disable the gate: the update then succeeds as soon as the new container has started. The gate adds at most `HEALTH_WAIT_SECONDS` to the update of each container.

Details:

- A healthcheck verdict takes time. With Docker's default settings (interval 30 s, 3 retries, plus the `start_period`), a broken but still running application is only reported `unhealthy` after about 90 s. Set `HEALTH_WAIT_SECONDS` to at least `start_period + interval * retries` to catch such an application. Crashes and restarts are always caught within a few seconds during the gate, whatever the value.
- A container whose `Health` status is empty or `none` is treated as having no healthcheck.
- A failed inspection on the very last poll does not fail the gate: the last valid observation is used.
- A restart count above 0 counts, even if the restart happened before the first observation. The 10 seconds of stability start at the first successful observation.
- A defective image is not retried: after a gate rollback the updater remembers the pair (container, new image ID) until it restarts, and later scans (including "Scan now") skip that container while the pulled image stays the same, logging "skipping image that previously failed the health gate" without a new event or notification. A different new image ID resets it. Restart the updater to force a retry.
- Shutdown: if the updater is asked to stop while the gate is running, the gate is abandoned and the update is rolled back at once, with the reason "updater stopping during health gate", so the known-good version is running again quickly. Give the updater a grace period that covers a rollback (about 60 seconds, hence `stop_grace_period: 60s` and `--stop-timeout 60` in the examples above). If the process is killed in the middle of an update (SIGKILL after the grace period, a crash or a power loss), the old container is left stopped as `<name>_old` and the service may be down: start `<name>_old` or recreate the container by hand, then remove the leftover.

### Cleaning up old images

Pulled images pile up: every update leaves the previous image behind. With `CLEANUP_IMAGES=true`, after an update has passed the health gate and the old container has been removed, the updater asks the daemon to remove the image that old container ran, without `force`:

- the removal is best effort and never makes an update fail: the daemon refuses when another container still uses the image (for example several containers on the same image that are not updated yet, or an opted-out one), and the image is then simply kept for now;
- the image is not removed when it is the same as the new one;
- nothing is removed after a rollback, since the previous image is the one running again;
- if the old container could not be removed, its image is kept too;
- the old image is only removed when it carries no tag any more (it was untagged when the pull moved the tag). An image that still has a tag is kept.

In the integration test the updater removes the old image on both the classic and the containerd image stores. Should a store drop the old image on its own when the tag moves, there is nothing left to remove and the updater only logs it at debug level.

It is off by default.

### Locally built images

An image is considered local, and its containers are skipped, when it never came from a registry. How that is detected depends on the daemon's image store:

- **containerd image store** (the default on recent Docker installs, including Docker Desktop): the image description has an `Identity` section. The image is local when it has build records and no pull record. An image with a pull record, or with an empty `Identity`, is not local.
- **classic image store**: the image is local when it has no repo digest, which is the case for an image that was built or loaded and never pushed or pulled.

All the distinct images used by the containers of a given image reference are checked, and the group is skipped only if every one of them is local. If any image is not local, or if the check itself fails (a warning is then logged once per image), the image is pulled as usual: skipping by mistake would silently stop updates, so uncertainty always resolves to checking.

Known cases where the result may not be what you expect (the detection is a heuristic, so when in doubt use `docker-image-updater.enable=false` to exclude a container for sure, or push the image to a registry and run it from there):

- A registry image retagged with `docker tag` is still a registry image: it is pulled under its new name, which fails if that name does not exist in a registry (reported once, like any pull failure).
- Classic store: an image loaded with `docker load` looks local and is skipped. An image built locally and then pushed to your own registry has repo digests afterwards, so it is pulled normally.
- Containerd store: an image loaded with `docker load` or imported has neither build nor pull records, so it is not recognised as local: the updater tries to pull it and reports the failure once. Exclude such containers with `docker-image-updater.enable=false`.
- Containerd store: an image built locally keeps its build-only identity even after you push it, so it is skipped even though a newer version may exist in your registry. Pull it from the registry (`docker pull`) before running it, or accept that it is not updated.

## Private registries

The Docker daemon does not read `~/.docker/config.json` (only the Docker CLI does), so the updater sends the registry credentials itself with every pull. Give it a Docker config file:

1. Create a directory with a `config.json` file (example below).
2. Mount it read-only into the updater and point `DOCKER_CONFIG` to it:

```bash
docker run -d --name docker-image-updater --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /path/to/dockerconfig:/config:ro \
  -e DOCKER_CONFIG=/config \
  ghcr.io/bilyboy785/docker-image-updater:latest
```

Example `config.json`, with an entry for GitHub Container Registry and one for Docker Hub:

```json
{
  "auths": {
    "ghcr.io": { "auth": "dXNlcm5hbWU6dG9rZW4=" },
    "https://index.docker.io/v1/": { "auth": "dXNlcm5hbWU6dG9rZW4=" }
  }
}
```

`auth` is the base64 encoding of `username:token`:

```bash
printf '%s' 'username:token' | base64 | tr -d '\n'
```

Typing the token on the command line may leave it in your shell history: clear the history entry or read the token from a file or a password manager instead.

Entries may also use explicit `username` and `password` fields (when `auth` is also present it wins, as with the Docker CLI), or an `identitytoken`. For GHCR, use a personal access token with the `read:packages` scope. For Docker Hub, use an access token. A self-hosted registry is matched by its host and port, for example `registry.example.com:5000`.

Notes:

- Use a token with read-only scope: the updater only pulls.
- The file contains secrets. Make it `chmod 600`, owned by the user running the container (root), and mount it read-only, as shown above. The updater never logs the credentials or includes them in notifications.
- The file is read again on every pull, so a rotated token is picked up without restarting the updater. Mount the directory (not the file itself) and rotate atomically: write a temporary file in the directory, then rename it over `config.json`.
- The file is limited to 1 MiB.
- Registry names are matched case-insensitively, and a `https://` prefix or a path in the key is ignored. `docker.io`, `index.docker.io` and `registry-1.docker.io` all designate Docker Hub.
- Credential helpers and credential stores (`credsStore`, `credHelpers`) are not supported, because the image contains no helper binaries. On macOS and Windows, `docker login` stores the secrets in the OS keychain (`credsStore`), so the generated `~/.docker/config.json` has no usable `auths` entry: create a dedicated minimal file as shown above instead. When a registry is configured with a helper and has no inline entry, the updater logs a warning once and pulls anonymously.
- If the file is missing or invalid, the updater logs a warning and pulls anonymously.
- A pull rejected by the registry (for example `unauthorized`) is reported like any other pull failure, once per image and error.

## Opting out

Add the label `docker-image-updater.enable=false` to any container that must not be updated.

```bash
docker run -d --label docker-image-updater.enable=false nginx:latest
```

```yaml
services:
  web:
    image: nginx:latest
    labels:
      docker-image-updater.enable: "false"
```

### Update policy and windows

Two more labels control how a container is updated. They are evaluated once per container each time a newer image is found, before anything is renamed or stopped.

| Label | Values | Default |
|-------|--------|---------|
| `docker-image-updater.policy` | `auto`: recreate the container. `notify`: only report that an update is available. | `auto` |
| `docker-image-updater.window` | One or more `HH:MM-HH:MM` ranges separated by commas, for example `02:00-05:00` or `02:00-03:00,23:00-23:59`. | any time |

```yaml
services:
  database:
    image: postgres:16
    labels:
      docker-image-updater.policy: "notify"   # never recreated, you are told when an update exists
  web:
    image: nginx:latest
    labels:
      docker-image-updater.window: "02:00-05:00"   # updated at night only
  worker:
    image: ghcr.io/example/worker:latest
    labels:
      docker-image-updater.window: "22:00-24:00,00:00-02:00,12:00-13:00"   # several ranges; 24:00 is allowed as an end
  updater:
    image: ghcr.io/bilyboy785/docker-image-updater:latest
    environment:
      TZ: Europe/Paris   # windows are evaluated in this zone, UTC when unset
```

Details:

- Times use the 24-hour clock, `HH:MM` zero-padded. The start is inclusive and the end exclusive, so `02:00-05:00` covers 02:00 up to 04:59. A range whose end is before its start wraps past midnight. `24:00` is accepted as a range end only, so `22:00-24:00` covers the evening up to midnight. Spaces around ranges and times are tolerated.
- The clock is the updater's, in the zone given by the `TZ` environment variable (`UTC` by default). If `TZ` names an unknown zone the updater falls back to UTC and logs the warning `TZ value not recognised, using UTC` at startup. Mind daylight saving time: on the day clocks go forward, a window such as `02:00-03:00` does not exist in some zones, so the update is deferred to the next day.
- Windows are evaluated when a scan finds a newer image. Outside the window nothing happens: no event, no notification, and the update is applied by the first scan that falls inside the window. Make sure the scan interval is short enough to hit your windows. A forced "Scan now" also respects windows.
- With `notify`, one "Update available" event and one notification are sent per container and new image (until the updater restarts) and the container is never recreated. The window is ignored.
- An empty `policy` label means `auto`. An empty or blank `window` label is an error, as is any malformed range.
- Invalid labels (unknown policy, malformed window, a typo such as `notfy`) fail closed: the container is not updated, a warning is logged once per container and label values as soon as a scan sees them (even before any update exists), and no notification is sent. Fix the label to resume updates.
- Dry-run mode stays global and unchanged: it never recreates anything. The updater itself and `--rm` containers are handled as described above, whatever their labels.
- `docker-image-updater.enable=false` still excludes a container completely.

## Telegram setup

1. Open a chat with [@BotFather](https://t.me/BotFather) on Telegram, send `/newbot` and follow the steps. BotFather gives you a bot token.
2. Send a message to your new bot (or add it to a group), then open `https://api.telegram.org/bot<your-token>/getUpdates` in a browser and read the `chat.id` value.
3. Set `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID`. Both must be set together.

Messages are sent as HTML. Example:

```
✅ Updated nginx
Image: nginx:latest
1a2b3c4d5e6f → 7a8b9c0d1e2f
```

Notifications are queued and sent in the background, so a slow or failing Telegram never blocks or fails a scan:

- Requests are rate limited to about one every 1.2 seconds. During a burst (for example several updates in one scan), pending messages are merged into fewer messages, separated by a blank line, up to 4000 characters per message. Longer messages are truncated.
- When Telegram answers with a rate limit error, the `retry_after` delay it gives is honoured. Temporary errors (network, server) are retried up to 5 times with an increasing delay.
- Errors that cannot succeed on retry (for example a wrong chat ID or an invalid message) are logged and the message is dropped.
- The backlog is bounded to 200 messages. Beyond that the oldest ones are dropped and the next message starts with a notice of how many were dropped.
- On shutdown, pending messages are flushed on a best-effort basis for up to 10 seconds.
- Delivery is at-least-once: after an ambiguous network failure a notification may be delivered twice.

Other messages are "Update available" (dry-run, `notify` policy, the updater itself and `--rm` containers), "Update failed", "Update failed and was rolled back" or "Update rolled back" (the new container failed the health gate) and "Cannot check image" (pull failure).

## Web dashboard

Set `WEB_ENABLED=true` to start a small web server (address set by `WEB_ADDR`, `:8080` by default).

| Route         | Description |
|---------------|-------------|
| `/`           | Statistics page. |
| `/api/stats`  | The same statistics as JSON. |
| `/healthz`    | Returns `ok`. |
| `POST /api/scan` | Starts an immediate scan (see below). |

### Forcing a scan

The **Scan now** button in the page header starts a scan right away. While a scan runs the button is disabled and the page refreshes every second. The same action is available through the API, which needs a custom header:

```bash
curl -X POST -H 'X-Requested-With: docker-image-updater' http://127.0.0.1:8080/api/scan
```

The call returns immediately with `202` and `{"status":"started"}`. A forced scan restarts the schedule: the next automatic scan happens one interval after it ends. The endpoint is guarded:

- the `X-Requested-With: docker-image-updater` header is required, and an `Origin` header, when present, must match the `Host` of the request, otherwise the answer is `403`;
- only one scan runs at a time: `409` while one is running;
- a forced scan is refused with `429` and a `Retry-After` header when the last scan started less than 30 seconds ago (automatic scans are not affected);
- other methods answer `405`, and the request body is ignored.

**The dashboard has no authentication.** Bind it to `127.0.0.1` (for example `127.0.0.1:8080:8080` in the port mapping) or put it behind a reverse proxy that handles access control. The dashboard is no longer strictly read-only: anyone who can reach it can force a scan. This does nothing the schedule would not do anyway, but there is still no authentication, so keep it on a private network or behind a reverse proxy. The header and `Origin` checks do not protect against DNS rebinding: rely on network isolation and do not expose the port to untrusted networks.

Behind a reverse proxy, the proxy must forward the original `Host` header, because the `Origin` of the page is compared with it. With nginx add `proxy_set_header Host $host;` (the `proxy_pass` default sends the upstream host); Traefik and Caddy do it by default. Otherwise "Scan now" fails with HTTP 403.

## Security considerations

- Mounting `/var/run/docker.sock` gives the container root-equivalent access to the host. Only run images you trust.
- Because of this, the image runs as root.
- Keep the web port private (see above).

## Limitations

- Registry credential helpers and credential stores are not supported: put an inline `auth` entry in the Docker config file (see "Private registries").
- A container using `network_mode: container:X` is not re-linked when `X` is recreated: it is named in the notification and must be restarted manually.
- Containers started with `--rm` are not updated automatically (see above).
- Containers attached to several networks need Docker Engine API 1.44 or later (Docker 25+).
- Ordering between containers (`depends_on`) is not handled.
- The updater never updates itself, it only notifies.
- If an update is interrupted, a leftover `<name>_old` container must be removed manually.
- If the old container has a restart policy such as `always` and the Docker daemon restarts during the health gate, the daemon can start `<name>_old` again next to the new container (for example with a port conflict): stop and remove `<name>_old` manually.
- Static MAC addresses are not preserved.
- Only the Unix socket on Linux/Unix is supported.

## Development

Everything runs in Docker, no local Go toolchain is needed.

```bash
make test                      # full unit test suite
make test-pkg PKG=./internal/config/...   # a single package
make build                     # build the image as docker-image-updater:dev
make test-integration          # end-to-end test
```

The integration test runs in an isolated `docker:dind` daemon and never touches the containers of your local Docker daemon. Set `DIND_IMAGE` to choose the daemon, for example `make test-integration DIND_IMAGE=docker:29-dind`: `docker:27-dind` (the default) uses the classic image store and `docker:29-dind` the containerd one, and CI runs both. It uses a password-protected local registry, so it also covers private registry credentials. It requires Docker with support for privileged containers.

Layout:

- `cmd/updater`: entry point.
- `internal/config`: environment variables and flags.
- `internal/docker`: minimal Docker Engine API client.
- `internal/updater`: scan loop, recreation and rollback.
- `internal/registry`: registry credentials from a Docker config file.
- `internal/notify`: Telegram notifications.
- `internal/stats`: statistics and persisted state.
- `internal/web`: dashboard and API.
- `test`: integration test.

Every push to `main` and every pull request runs the unit and integration tests. To release, publish a GitHub release for a `v*` tag (for example `gh release create v1.0.0 --generate-notes`): the Release workflow re-runs the tests, then builds and pushes the multi-arch image to `ghcr.io`. Pre-releases (for example `v1.1.0-rc1`) do not update `latest`.

## License

[MIT](LICENSE)
