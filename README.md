# docker-image-updater

Keeps your running Docker containers up to date: it pulls fresh images on a schedule and recreates the containers that changed.

[![Release](https://img.shields.io/github/v/release/bilyboy785/docker-image-updater)](https://github.com/bilyboy785/docker-image-updater/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

## Features

- Automatic updates for every running container.
- Works with containers started by `docker run`, `docker compose` and `docker compose -p`.
- Periodic check, with the interval set in minutes.
- Private registry support (GHCR, Docker Hub, self-hosted) through a mounted Docker config file.
- Telegram notifications (optional).
- Optional statistics dashboard with the status of every container and buttons to force a scan, of everything or of a single container.
- Prometheus metrics (`/metrics`), a liveness endpoint (`/healthz`) and a built-in image `HEALTHCHECK`.
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
docker run -d --name docker-image-updater --restart unless-stopped --stop-timeout 90 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v updater-data:/data \
  -e CHECK_INTERVAL_MINUTES=60 \
  -e STATE_FILE=/data/state.json \
  registry.bldwebagency.fr/docker-image-updater:latest
```

With Docker Compose:

```yaml
services:
  docker-image-updater:
    image: registry.bldwebagency.fr/docker-image-updater:latest
    restart: unless-stopped
    stop_grace_period: 90s   # the update in progress (a rollback takes up to about 60 s) and the notification flush (up to 10 s) must fit (docker stop waits 10 s by default)
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
| none                     | `-healthcheck`     | off                     | Probe `/healthz` of the local web server and exit (`0` healthy, `1` otherwise), then stop. Used by the image `HEALTHCHECK`; exits `0` at once when the web server is disabled. See [Health endpoint and image healthcheck](#health-endpoint-and-image-healthcheck). |

## How it works

On each scan the updater lists the running containers and, for every distinct image reference, pulls the image and compares the resulting image ID with the one each container is running. A container whose image ID differs is updated.

Containers that are skipped:

- containers whose image is pinned by digest (`image@sha256:...`);
- containers opted out with the `docker-image-updater.enable=false` label (see below). They are never updated, but they are still listed as `skipped` in the dashboard and in the API (`/api/stats`);
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
- An image reference without a tag (`image: nginx`, `ghcr.io/owner/app`) is pulled as `:latest`, like `docker pull` does, so only that one tag is downloaded, never the whole repository. The reference stored in the container is left untouched. `nginx` and `nginx:latest` are checked as two separate references, which only costs one extra (cheap) pull.
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
- Shutdown: if the updater is asked to stop while the gate is running, the gate is abandoned and the update is rolled back at once, with the reason "updater stopping during health gate", so the known-good version is running again quickly. Give the updater a grace period that covers a rollback (up to about 60 seconds, plus up to 10 seconds to flush the notifications, hence `stop_grace_period: 90s` and `--stop-timeout 90` in the examples above). If the process is killed in the middle of an update (SIGKILL after the grace period, a crash or a power loss), the old container is left stopped as `<name>_old` and the service may be down: start `<name>_old` or recreate the container by hand, then remove the leftover.

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
  registry.bldwebagency.fr/docker-image-updater:latest
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

Two more labels control how a container is updated. They are evaluated at every scan, for the container list and the report (an invalid label shows as `skipped`), and again when a newer image is found, before anything is renamed or stopped.

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
    image: registry.bldwebagency.fr/docker-image-updater:latest
    environment:
      TZ: Europe/Paris   # windows are evaluated in this zone, UTC when unset
```

Details:

- Times use the 24-hour clock, `HH:MM` zero-padded. The start is inclusive and the end exclusive, so `02:00-05:00` covers 02:00 up to 04:59. A range whose end is before its start wraps past midnight. `24:00` is accepted as a range end only, so `22:00-24:00` covers the evening up to midnight. Spaces around ranges and times are tolerated.
- The clock is the updater's, in the zone given by the `TZ` environment variable (`UTC` by default). If `TZ` names an unknown zone the updater falls back to UTC and logs the warning `TZ value not recognised, using UTC` at startup. Mind daylight saving time: on the day clocks go forward, a window such as `02:00-03:00` does not exist in some zones, so the update is deferred to the next day.
- Windows are evaluated when a scan finds a newer image. Outside the window nothing happens: no event, no notification, and the update is applied by the first scan that falls inside the window. Make sure the scan interval is short enough to hit your windows. A forced "Scan now" also respects windows.
- With `notify`, one "Update available" event and one notification are sent per container and new image (until the updater restarts) and the container is never recreated. The window is ignored.
- An empty `policy` label means `auto`. An empty or blank `window` label is an error, as is any malformed range.
- Invalid labels (unknown policy, malformed window, a typo such as `notfy`) fail closed: the container is not updated, a warning is logged once per container and label values as soon as a scan sees them (even before any update exists), and no notification is sent (the updater's own container is still announced). Such a container counts in `watched` (`diu_containers_watched`) but appears as `skipped` in the container list and in `diu_containers`. Fix the label to resume updates.
- Dry-run mode stays global and unchanged: it never recreates anything. The updater itself and `--rm` containers are handled as described above, whatever their labels.
- `docker-image-updater.enable=false` still excludes a container: it is never updated (and still listed as `skipped` in the dashboard).

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
| `/`           | Statistics page, with the container list. |
| `/api/stats`  | The same statistics as JSON, including the container list (`containers`). |
| `/metrics`    | Prometheus metrics (see [Metrics](#metrics)). |
| `/healthz`    | `200 ok` while the scan loop is alive, `503 stale: ...` otherwise (see [Health endpoint and image healthcheck](#health-endpoint-and-image-healthcheck)). |
| `POST /api/scan` | Starts an immediate scan, of every container or of one (`?container=<name>`, see below). |

### Container list

The **Containers** section of the page, and the `containers` array of `/api/stats`, show what the last scan found about every running container, with one entry per container name. Chips above the table filter by status (click a chip again to clear the filter). The list is rebuilt by each full scan (a scan of a single container only replaces its own entry, see [Scanning one container](#scanning-one-container)) and is not saved in the state file, so it is empty until the first scan after a start.

| Status             | Meaning |
|--------------------|---------|
| `up-to-date`       | The container runs the image the registry currently serves. |
| `updated`          | The container was recreated on a newer image by the last scan (the detail shows the old and new image ids, and a health note if any). |
| `update-available` | A newer image exists but is not applied: `policy: notify`, dry-run, a `--rm` container, or the updater itself (manual update required). |
| `waiting-window`   | A newer image exists and the update waits for the container's update window. |
| `failed`           | The image could not be checked (pull or lookup error) or the update failed. The detail holds the error, cut to 200 characters. |
| `rolled-back`      | The update was rolled back (health gate failure), or the image is not retried because of an earlier gate failure. |
| `skipped`          | Not checked: opted out, pinned by digest, image reference that cannot be resolved, locally built image, or invalid policy labels. The detail says which. |
| `self`             | The updater's own container, which is never updated by itself. |

Each entry also carries the image, its short image id, the policy (`auto`, `notify` or `invalid`), the window label (cut to 64 characters) and the time of the check.

### Metrics

`GET /metrics` serves the counters and gauges below in the Prometheus text format (version 0.0.4). They come from the same data as `/api/stats`; no container name, image or environment is exposed.

| Metric | Type | Description |
|--------|------|-------------|
| `diu_scans_total` | counter | Completed scans (kept across restarts with `STATE_FILE`). |
| `diu_updates_total` | counter | Containers updated. |
| `diu_failures_total` | counter | Failed updates and image checks. |
| `diu_rollbacks_total` | counter | Updates rolled back. |
| `diu_scanning` | gauge | `1` while a scan runs, else `0`. |
| `diu_containers_watched` | gauge | Containers checked by the last scan. |
| `diu_containers_skipped` | gauge | Containers ignored by the last scan. |
| `diu_last_scan_timestamp_seconds` | gauge | Start of the last scan as a Unix time, `0` when none yet. |
| `diu_last_scan_duration_seconds` | gauge | Duration of the last scan. |
| `diu_next_scan_timestamp_seconds` | gauge | Planned time of the next scan, `0` when unknown. |
| `diu_uptime_seconds` | gauge | Time since the updater started. |
| `diu_build_info{version,mode}` | gauge | Always `1`; the labels carry the version and the mode (`active` or `dry-run`). |
| `diu_containers{status}` | gauge | Containers per status of the last scan, for every status above (`0` when none). |

A scrape configuration:

```yaml
scrape_configs:
  - job_name: docker-image-updater
    static_configs:
      - targets: ["updater-host:8080"]
```

### Health endpoint and image healthcheck

`/healthz` answers `200` with `ok` while the scan loop is alive and `503` with a short `stale: ...` text when it is not. The reference time is the latest of three moments: the end of the last scan completed by this process, the last progress of the scan in progress (the scan records a heartbeat at its start, around every pull and image lookup, and at every container step) and the start of the process. The updater is healthy while `now - reference <= 2 * interval + 35 minutes` (35 minutes cover the 30 minute pull timeout of one slow step). In other words, `/healthz` reports `stale` only when no scan progress was seen for `2 x interval + 35 minutes`: a long scan that keeps working stays healthy, so an orchestrator that restarts unhealthy containers (autoheal, Swarm) does not restart the updater in the middle of an update, while a scan that is really stuck is reported. The heartbeat is also exposed as `last_progress` in `/api/stats` (never persisted).

The image declares `HEALTHCHECK --interval=60s --timeout=5s --start-period=30s --retries=3 CMD ["/docker-image-updater","-healthcheck"]`. The `-healthcheck` mode requests `/healthz` on `WEB_ADDR` (`:8080` becomes `127.0.0.1:8080`; a host is kept), with a 3 second timeout, and exits `0` on a `200` and `1` otherwise. When the web server is disabled (`WEB_ENABLED` not true) there is nothing to check and it exits `0` immediately, so there is nothing to fail. Docker only runs the first probe after the first `--interval` though: with the web server disabled `docker ps` shows `health: starting` for the first minute, then `healthy`. With the web server enabled, `docker ps` shows `(healthy)` after the first successful probe, also one minute after the start.

The probe ignores proxy variables (it always talks to the local server directly, so `NO_PROXY` is not needed) and does not follow redirects: anything but a `200` is unhealthy. A configuration error in `-healthcheck` mode also exits `1`.

The image `HEALTHCHECK` reads environment variables only, never command-line flags. If you configure the updater with flags such as `-web-addr :9090`, the probe still targets `WEB_ADDR` (`:8080` by default) and would report the container unhealthy. Override the healthcheck in compose so that it gets the same flags:

```yaml
services:
  docker-image-updater:
    image: registry.bldwebagency.fr/docker-image-updater:latest
    command: ["-web", "true", "-web-addr", ":9090"]
    healthcheck:
      test: ["CMD", "/docker-image-updater", "-healthcheck", "-web", "true", "-web-addr", ":9090"]
      interval: 60s
      timeout: 5s
      start_period: 30s
      retries: 3
```

Setting `WEB_ENABLED` and `WEB_ADDR` as environment variables instead needs no override.

**`/metrics`, `/api/stats` and the page have no authentication**, like the rest of the dashboard: see the note below.

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

**The dashboard has no authentication** (neither has `/metrics`, `/api/stats` nor `/healthz`). Bind it to `127.0.0.1` (for example `127.0.0.1:8080:8080` in the port mapping) or put it behind a reverse proxy that handles access control. The dashboard is no longer strictly read-only: anyone who can reach it can force a scan. This does nothing the schedule would not do anyway, but there is still no authentication, so keep it on a private network or behind a reverse proxy. The header and `Origin` checks do not protect against DNS rebinding: rely on network isolation and do not expose the port to untrusted networks.

Behind a reverse proxy, the proxy must forward the original `Host` header, because the `Origin` of the page is compared with it. With nginx add `proxy_set_header Host $host;` (the `proxy_pass` default sends the upstream host); Traefik and Caddy do it by default. Otherwise "Scan now" fails with HTTP 403.

### Scanning one container

Every row of the container table has a **Scan** button (column **Action**) that scans that container only. The buttons are disabled while any scan runs. The updater's own row and the skipped rows have the button too: scanning them is harmless (the updater only announces a newer image of itself, a skipped container only shows again why it is skipped). The API equivalent takes the container name in the `container` parameter (names are made of letters, digits, `_`, `.` and `-`, so they rarely need URL-encoding):

```bash
curl -X POST -H 'X-Requested-With: docker-image-updater' 'http://127.0.0.1:8080/api/scan?container=my-app'
# container names rarely need encoding; this is only a precaution
curl -X POST -H 'X-Requested-With: docker-image-updater' --get --data-urlencode 'container=my-app_1.web' http://127.0.0.1:8080/api/scan
```

Without the parameter the call is the full scan described above. The answer is `202` with `{"status":"started","container":"my-app"}`. The guards of the full scan apply (header, `Origin`, `POST` only, `409` while a scan runs), plus:

- the name must be the name of a container of the last published container list, compared as is: `404` otherwise (the page then asks you to refresh it). A malformed name (empty or blank, longer than 255 bytes, invalid UTF-8, a control character or `/`, or several `container` parameters) is a `400`;
- a targeted scan is refused with `429` and a `Retry-After` header when the last scan, of either kind, started less than 5 seconds ago. A full forced scan keeps its 30 seconds, also measured from the start of the last scan of either kind, so the stricter limit of the requested kind applies.

The targeted scan runs exactly the same checks as a full scan for that container, and nothing for the others (no pull, no inspection): opt-out, digest, locally built image, `policy=notify`, update windows (a container outside its window is **not** updated: its row says `waiting-window`), invalid labels, `AutoRemove`, dry-run, the health gate with rollback and the memory of images that failed it (shared with the full scans), image cleanup, notifications and events. If the container disappeared since the last scan, nothing is scanned (it is logged, not notified) and its row is removed from the list, so it cannot be clicked again.

A targeted scan counts in `scans`, `last_scan` and the duration, but it does not move the periodic schedule (a full forced scan does), and it replaces only the entry of that container in the container list: the other entries keep their status and their last check time. The `watched` and `skipped` gauges describe whole scans and keep their previous values after a targeted scan.

## Security considerations

- Mounting `/var/run/docker.sock` gives the container root-equivalent access to the host. Only run images you trust.
- Because of this, the image runs as root.
- Keep the web port private (see above).

The Dockerfile base image is pinned by digest and Dependabot proposes weekly updates for it.

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

The integration test runs in an isolated `docker:dind` daemon and never touches the containers of your local Docker daemon. Set `DIND_IMAGE` to choose the daemon, for example `make test-integration DIND_IMAGE=docker:29-dind`: `docker:27-dind` (the default) uses the classic image store and `docker:29-dind` the containerd one. It uses a password-protected local registry, so it also covers private registry credentials. It requires Docker with support for privileged containers.

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

There is no hosted CI. Before each release, `./scripts/publish.sh` runs the unit and integration tests locally in Docker, then builds the `linux/amd64` and `linux/arm64` image and pushes it to `registry.bldwebagency.fr/docker-image-updater` as `latest`, plus the version tag (without the `v` prefix) when HEAD is on a `v*` tag. To release, tag the commit, push the tag, run the script and publish the GitHub release (`gh release create v1.0.0 --generate-notes`).

## License

[MIT](LICENSE)
