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

Every setting can be given as an environment variable or as a command-line flag. A flag overrides the environment variable.

| Environment variable     | Flag               | Default                 | Description |
|--------------------------|--------------------|-------------------------|-------------|
| `CHECK_INTERVAL_MINUTES` | `-interval`        | `60`                    | Time between two scans, in minutes (integer, at least 1). |
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
- containers opted out with the `docker-image-updater.enable=false` label (see below);
- the updater itself, identified by its hostname or by the `docker-image-updater.self=true` label. It never updates itself: it only sends a notification.

To update a container, the updater inspects it and recreates it from that description:

1. rename the old container to `<name>_old`;
2. stop it;
3. create a new container with the original name and configuration, running the new image;
4. connect the additional networks;
5. start the new container;
6. remove the old container.

If any step before the removal fails, the new container (if any) is removed, the old one is renamed back and started again, and the failure is reported as a rollback.

Because the new container is rebuilt from `docker inspect`, this works the same for `docker run`, `docker compose` and `docker compose -p` containers:

- all labels are preserved, including the `com.docker.compose.*` ones, and the `com.docker.compose.image` label is refreshed to the new image ID so that a later `docker compose up` does not recreate the container again;
- anonymous volumes are re-bound to the new container, so their data is kept;
- networks, aliases and static IP settings are carried over;
- with `network_mode: container:X`, the hostname is not set on the new container, as Docker forbids it.

In dry-run mode, and for the updater itself, an "update available" notification is sent once per new image.

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

Other messages are "Update available" (dry-run and the updater itself), "Update failed" and "Update failed and was rolled back".

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
- A container using `network_mode: container:X` is not re-linked when `X` is recreated.
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
