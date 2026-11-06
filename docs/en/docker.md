# Docker deployment

[← Docs home](README.md)　·　[← Back to the project README](../../README.en.md)　·　[中文](../docker.md)　·　English

The image bundles the program, the web UI and its runtime dependencies. It fits a NAS, a VPS, or any machine already managed by Docker. Linux containers only.

## Quick start

```bash
mkdir -p wen-data
sudo chown 10001:10001 wen-data
export WEN_AUTH_PASSWORD='choose a long password'
docker compose up -d --build
```

Open `http://127.0.0.1:8080`. On first run there is no usable model yet, so run the setup wizard once:

```bash
docker compose run --rm -it wen onboard -c /data/config.yaml
```

`WEN_AUTH_PASSWORD` is required. The container listens on `0.0.0.0`, and the program deliberately falls back to `127.0.0.1` when it is exposed without a password — without this variable the container starts but nothing outside it can reach the UI.

The data directory must be owned by uid 10001, because the image runs as a non-root user. Docker creates `wen-data` as root if it does not exist, and then the program cannot write to it. On Linux, create it and change ownership as shown above; Docker Desktop usually maps it for you.

## Prebuilt images and multiple architectures

Stable releases publish amd64 and arm64 images under one tag, and Docker picks the architecture that matches the machine:

```bash
docker pull ghcr.io/lprensoft/wen-agent:latest
```

The package is private by default. To pull it anonymously elsewhere, make it public in the package settings on GitHub, or run `docker login ghcr.io` first. Running the "Docker image" workflow manually publishes a `dev` tag for validating the pipeline before a release.

compose builds locally by default. To use the prebuilt image:

```bash
export WEN_IMAGE=ghcr.io/lprensoft/wen-agent:latest
docker compose pull
docker compose up -d
```

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `WEN_AUTH_PASSWORD` | none (required) | Web UI access password |
| `WEN_PORT` | `8080` | Host port to publish; the container always listens on 8080 |
| `TZ` | `Asia/Shanghai` | Time zone; schedules, day boundaries and weather depend on local time |
| `WEN_VERSION` | empty | Version stamped at build time; falls back to the version in the source |
| `WEN_IMAGE` | `wen-agent:local` | Image to use; set it to `ghcr.io/lprensoft/wen-agent:latest` for the prebuilt image |

The configuration file, sessions and plugin data all live under `/data`, which compose maps to `./wen-data`. Back up that directory and you have backed up everything.

## Updating

```bash
git pull
docker compose up -d --build
```

Do not use `self_update` inside the container. It replaces the binary inside the image, which disappears when the container is recreated, and `/usr/local/bin` is not writable by the runtime user anyway. Turn "Maintenance · Program update" off on the settings page and update through images instead.

## Security

- The image listens on `0.0.0.0` and requires a password, but still do not expose the port directly to the internet.
- TLS is not provided by this program. Behind a reverse proxy, turn off `server.trust_loopback` — see [Deployment and access control](deployment.md).
- The container only carries Alpine's base environment. An MCP server inside an external bundle that needs node, python or similar must be installed in a derived image, or reached over `streamable-http` from outside the container.
- `exec_command` and `read_file` are off by default. Once enabled, files and commands inside the container are available to the model; running as non-root limits the impact but is not a sandbox.

## Building it yourself

```bash
docker build -t wen-agent:local .
docker build --build-arg VERSION=v1.0.0 -t wen-agent:v1.0.0 .
```

compose uses the locally built `wen-agent:local` by default. To use the prebuilt image, override `WEN_IMAGE` as described under "Prebuilt images and multiple architectures" above.
