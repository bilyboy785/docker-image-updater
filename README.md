# docker-image-updater

Keeps your running Docker containers up to date: it pulls fresh images on a schedule and recreates the containers that changed.

[![CI](https://github.com/bilyboy785/docker-image-updater/actions/workflows/ci.yml/badge.svg)](https://github.com/bilyboy785/docker-image-updater/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Image](https://img.shields.io/badge/ghcr.io-docker--image--updater-informational)](https://github.com/bilyboy785/docker-image-updater/pkgs/container/docker-image-updater)

## Features

- Automatic updates for every running container.
- Works with containers started by `docker run`, `docker compose` and `docker compose -p`.
- Periodic check, with the interval set in minutes.
- Telegram notifications (optional).
- Optional statistics dashboard.
- Per-container opt-out label.
- Dry-run mode: only notify, never recreate.
- Automatic rollback when an update fails.
- Tiny `scratch` image (about 3 MB), multi-arch (`linux/amd64`, `linux/arm64`).
- No third-party Go dependencies.

## Quick start

With `docker run`:

```bash
docker run -d --name docker-image-updater --restart unless-stopped \
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
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - updater-data:/data
    environment:
      CHECK_INTERVAL_MINUTES: "60"
      TELEGRAM_BOT_TOKEN: "123456:ABC-your-bot-token"
      TELEGRAM_CHAT_ID: "123456789"
      WEB_ENABLED: "true"
      STATE_FILE: /data/state.json
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

## How it works

On each scan the updater lists the running containers and, for every distinct image reference, pulls the image and compares the resulting image ID with the one each container is running. A container whose image ID differs is updated.

Containers that are skipped:

- containers whose image is pinned by digest (`image@sha256:...`);
- containers opted out with the `docker-image-updater.enable=false` label (see below).

The updater itself (identified by its hostname or by the `docker-image-updater.self=true` label) is watched and notified about, but it is never recreated: update it manually. Containers started with `--rm` (AutoRemove) are handled the same way, see below.

To update a container, the updater inspects it and recreates it from that description:

1. rename the old container to `<name>_old`;
2. stop it;
3. create a new container with the original name and configuration, running the new image;
4. connect the additional networks;
5. start the new container;
6. remove the old container.

If a step fails after the old container has been renamed and stopped (steps 2 to 5), the new container (if any) is removed, the old one is renamed back and started again, and the failure is reported as a rollback. A failure before that point (for example when the container cannot be inspected or renamed) leaves the old container untouched and is reported as failed. If the rollback itself fails, the error says that the service may be down and names the stopped old container (`<name>_old`, or `<name>` if only the restart failed) so you can start it by hand.

Because the new container is rebuilt from `docker inspect`, this works the same for `docker run`, `docker compose` and `docker compose -p` containers:

- all labels are preserved, including the `com.docker.compose.*` ones, and the `com.docker.compose.image` label is refreshed to the new image ID so that a later `docker compose up` does not recreate the container again;
- anonymous volumes are re-bound to the new container, so their data is kept;
- networks, aliases and static IP settings are carried over;
- with `network_mode: container:X`, the hostname is not set on the new container, as Docker forbids it;
- the image configuration is not inherited from the old image: `docker inspect` merges the image defaults with the container settings, so the updater drops the values that merely repeat the old image's defaults (environment variables, labels, command, entrypoint, working directory, user, stop signal, healthcheck, exposed ports and volumes). The defaults of the new image take effect, while everything you set yourself (your own variables, labels, command and so on) is kept. If the old image is already gone, the inspected configuration is used as is.

Other behaviours:

- In dry-run mode, for the updater itself and for containers started with `--rm`, an "update available" notification is sent once per container and new image, until the updater restarts.
- Containers started with `--rm` (AutoRemove) are never touched: stopping them would make Docker delete them, so no rollback would be possible. The updater only notifies you, and you must update them manually.
- If pulling an image fails (for example a private image or a registry outage), the failure is logged, counted and notified once per image and error, not on every scan.
- When the updated container is the network provider of other containers (`network_mode: container:X`), the success notification names those containers: they share its network stack and must be restarted or recreated manually.
- Docker calls have timeouts (1 minute, and 30 minutes for pulls), so a stuck daemon cannot block the scan loop forever.

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

Other messages are "Update available" (dry-run, the updater itself and `--rm` containers), "Update failed", "Update failed and was rolled back" and "Cannot check image" (pull failure).

## Web dashboard

Set `WEB_ENABLED=true` to start a small web server (address set by `WEB_ADDR`, `:8080` by default).

| Route         | Description |
|---------------|-------------|
| `/`           | Statistics page. |
| `/api/stats`  | The same statistics as JSON. |
| `/healthz`    | Returns `ok`. |

**The dashboard has no authentication.** Bind it to `127.0.0.1` (for example `127.0.0.1:8080:8080` in the port mapping) or put it behind a reverse proxy that handles access control.

## Security considerations

- Mounting `/var/run/docker.sock` gives the container root-equivalent access to the host. Only run images you trust.
- Because of this, the image runs as root.
- Keep the web port private (see above).

## Limitations

- Private registries that need credentials are not supported: images are pulled without registry authentication.
- A container using `network_mode: container:X` is not re-linked when `X` is recreated: it is named in the notification and must be restarted manually.
- Containers started with `--rm` are not updated automatically (see above).
- Locally built images (compose `build:`, `docker build -t name`) should be opted out with the label: the updater pulls the same name from the registry, which would replace them with a different image or fail.
- Containers attached to several networks need Docker Engine API 1.44 or later (Docker 25+).
- Ordering between containers (`depends_on`) is not handled.
- The updater never updates itself, it only notifies.
- If an update is interrupted, a leftover `<name>_old` container must be removed manually.
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

The integration test runs in an isolated `docker:dind` daemon and never touches the containers of your local Docker daemon. It requires Docker with support for privileged containers.

Layout:

- `cmd/updater`: entry point.
- `internal/config`: environment variables and flags.
- `internal/docker`: minimal Docker Engine API client.
- `internal/updater`: scan loop, recreation and rollback.
- `internal/notify`: Telegram notifications.
- `internal/stats`: statistics and persisted state.
- `internal/web`: dashboard and API.
- `test`: integration test.

To release, push a `v*` tag (for example `v0.1.0`). CI then builds and publishes the multi-arch image to `ghcr.io`.

## License

[MIT](LICENSE)
