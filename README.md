# Apperator

Apperator runs apps in Docker containers on your own server, behind a small REST API. You send it an image, environment variables and a hostname. It creates the container, routes the hostname to it, keeps its data, stops it when nobody uses it, and starts it again on the next request or when the app asks to be woken.

It is built for running many copies of the same app, one per customer, on one machine: SaaS products where each customer gets their own instance.

```bash
curl -X PUT http://127.0.0.1:7070/v1/apps/acme \
  -H "Authorization: Bearer $APPERATOR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "image": "ghcr.io/example/app:1.4",
    "env": { "PUBLIC_URL": "https://acme.example.com" },
    "hostnames": ["acme.example.com"],
    "port": 3000,
    "health_path": "/health",
    "data_path": "/data",
    "memory_mb": 384,
    "idle_timeout": "15m",
    "wake_file": "/data/next-work-at"
  }'
```

The call returns when `/health` answers. `acme.example.com` now reaches the app. After 15 minutes without requests the container stops and uses no memory. The next request starts it again in under a second.

## Why

Running one container per customer is simple and safe: no shared database, no noisy neighbours inside one process, and a customer's data is one volume. The hard part is running hundreds of them. Most of them are idle most of the time, and an idle container still uses 50–150 MB of memory.

Apperator stops idle apps and wakes them when they are needed. It is a thin layer over tools that already do their part well:

- **Docker** runs the containers.
- **[Traefik](https://traefik.io)** routes each hostname to its container.
- **[Sablier](https://github.com/sablierapp/sablier)** stops idle containers and holds the first request while a sleeping one starts.

Apperator adds what's missing: an API to create and change apps, Traefik's routes and Sablier's labels, health checks, scheduled wake-ups, a cap on how many apps start at once, and webhooks.

It knows nothing about your customers, plans or billing. Your own control plane calls the API.

## Features

- **Declarative.** `PUT` the app you want. Apperator creates the container, or recreates it when the image or settings changed. The same request twice changes nothing.
- **Data survives.** Each app has a named volume, kept when the container is recreated or the app is deleted. Deleting the data is a separate call.
- **Scale to zero.** With `idle_timeout`, Sablier stops the app after that long without requests and starts it on the next one.
- **Exact wake-ups.** A sleeping app can't run its own timers. With `wake_file`, the app writes the next time it has work (an RFC 3339 time) to a file in its data folder. Apperator reads it from the stopped container and starts the app at that time. As a safety net, sleeping apps also wake once a day.
- **Safe starts.** Apps start a few at a time (`APPERATOR_MAX_STARTING`), so a reboot or a burst of wake-ups can't use up the host's memory. Apps with an idle timeout don't restart on their own after a reboot: they wait for a request.
- **Health checks.** `PUT` and `start` wait until `health_path` answers, and so does a request that wakes a sleeping app. Apperator checks it with its own small binary inside the container, so this works in any image. If the app exits or never gets healthy, the error includes its last log lines.
- **One-off commands.** `POST /v1/apps/{id}/exec` runs a command in the app, starting it first if it is asleep.
- **Webhooks.** Signed events when apps are created, started, stopped, run out of memory, fail to start or are deleted.
- **Small.** One static Go binary with no dependencies outside the standard library. The image is about 10 MB and uses about 12 MB of memory.

## Quick start

You need a Linux server with Docker 25 or later, and a domain whose DNS points at it.

```bash
git clone https://github.com/automazeio/apperator
cd apperator
export APPERATOR_TOKEN=$(openssl rand -hex 32)
export ACME_EMAIL=you@example.com APPERATOR_CERT_RESOLVER=letsencrypt
docker compose -f deploy/compose.yaml up -d
```

This starts Traefik on ports 80 and 443 (with Let's Encrypt certificates), Sablier, and Apperator. The API listens on `127.0.0.1:7070`. Keep it private: reach it over a VPN or an SSH tunnel, never from the open internet.

Then create an app with the `curl` call above. See [docs/api.md](docs/api.md) for every endpoint and field.

To try it on your laptop without HTTPS:

```bash
APPERATOR_ENTRYPOINTS=web APPERATOR_TLS=false docker compose -f deploy/compose.yaml up -d
```

Hostnames under `.localhost`, such as `demo.localhost`, resolve to your machine in most browsers.

## How waking works

Apperator writes Traefik's routes to a file, and Traefik picks up each change within milliseconds. Each route points at the app's name on the Docker network, such as `http://apperator-acme:3000`. That address stays the same while the app sleeps, so Traefik doesn't need to reload when it wakes.

1. A request arrives for a sleeping app. Traefik passes it to the Sablier plugin.
2. Sablier starts the container and holds the request until Docker reports it healthy. With `health_path`, Apperator's own probe does that check; without it, the image's own health check does, and with neither, the request goes through as soon as the container runs, which can be before the app listens. Docker runs the check every 250 ms while the app starts, and the Compose stack has Sablier look every 250 ms (both default to 5 s).
3. The request goes on to the app. The client sees a slow response, not a redirect or an error.

How long this takes depends mostly on how fast your app boots. A Node.js app that boots in about 1.6 s answered its first request in about 2.2 s on a 2-core server. Several apps waking at once share the CPU, so six of them took about 14 s each on the same server. A new app gets its route as soon as `PUT` has created it.

Scheduled wake-ups go through Sablier's API too, so Sablier's idle timer applies to them in the same way.

## Configuration

Apperator reads its settings from environment variables.

| Variable | Default | Description |
|---|---|---|
| `APPERATOR_TOKEN` | – | Required. The API's bearer token, at least 32 characters. |
| `APPERATOR_LISTEN` | `:7070` | The API's address. |
| `APPERATOR_DATA_DIR` | `/var/lib/apperator` | Where app specs are kept (one JSON file per app). They include environment variables, so keep the folder private. |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | The Docker daemon. |
| `APPERATOR_NETWORK` | `apperator` | The Docker network shared by apps, Traefik and Sablier. Created if missing. |
| `APPERATOR_PREFIX` | `apperator-` | Prefix for container and volume names. App `acme` runs as `apperator-acme` with the volume `apperator-acme-data`. |
| `APPERATOR_ROUTER` | `traefik` | `traefik` writes routes for Traefik's file provider. `none` writes none, for running your own proxy. |
| `APPERATOR_ROUTES_FILE` | `/routes/apperator.yml` | Where Apperator writes Traefik's routes. Share its folder with Traefik (`--providers.file.directory`, `--providers.file.watch=true`). The name must end in `.yml` or `.yaml`. |
| `APPERATOR_ENTRYPOINTS` | `websecure` | Traefik entrypoints for app routes. |
| `APPERATOR_TLS` | `true` | Serve app routes over TLS. |
| `APPERATOR_CERT_RESOLVER` | – | Traefik certificate resolver, for example `letsencrypt`. |
| `APPERATOR_FALLBACK_URL` | – | Where requests for a hostname with no app are redirected, for example a deleted app's old address. Off when empty. HTTPS needs a certificate Traefik already has, such as a wildcard. |
| `APPERATOR_SABLIER_URL` | – | Sablier's address, for example `http://sablier:10000`. Required for `idle_timeout`. |
| `APPERATOR_SABLIER_TIMEOUT` | `30s` | How long a request waits for a sleeping app to start. |
| `APPERATOR_START_TIMEOUT` | `60s` | How long a start may take before it fails. |
| `APPERATOR_STOP_TIMEOUT` | `20` | Seconds an app has to shut down before it is killed. |
| `APPERATOR_MAX_STARTING` | `4` | How many apps may start at once. |
| `APPERATOR_MAX_RUNNING` | `0` | Scheduled wake-ups wait while this many apps run. `0` means no cap. |
| `APPERATOR_SAFETY_WAKE` | `24h` | Wake a sleeping app this long after it stopped, even without a wake file. `0` turns it off. |
| `APPERATOR_TICK` | `30s` | How often wake-ups are checked. |
| `APPERATOR_WEBHOOK_URL` | – | Where events are sent. |
| `APPERATOR_WEBHOOK_SECRET` | – | Signs events. Required with a webhook URL. |
| `APPERATOR_PROBE_BINARY` | this binary | A static Linux build of Apperator, copied into apps with `health_path` to run their health check. `off` turns this off. The official image needs no change. |
| `APPERATOR_REGISTRY_AUTH` | – | An `X-Registry-Auth` value for pulling private images. |
| `APPERATOR_LOG_FORMAT` | `text` | `text` or `json`. |

## When Apperator is down

Apperator only manages apps. Traefik and Sablier serve them. While Apperator is down or restarting, for example during an upgrade, running apps keep answering, and requests still wake sleeping apps and they still stop when idle. The routes file stays where it is, even if Traefik restarts. Only the API, scheduled wake-ups and webhooks wait until Apperator is back.

To catch a bad setting before it stops Apperator from starting, run `check` with the new settings before you deploy. It tests the settings, Docker and the folders, and exits:

```bash
docker compose -f deploy/compose.yaml run --rm --no-deps apperator check
```

## Security

- The API can run any image with any settings on your server, which is close to root access. Treat the token like a root password and keep the API off the public internet.
- Apperator and Sablier need the Docker socket. Traefik doesn't: it reads its routes from a file, through a read-only mount.
- Every hostname that gets its own certificate is published in public Certificate Transparency logs, and bots start scanning it within minutes. Each scan wakes a sleeping app. A wildcard certificate (a DNS challenge in Traefik) keeps your customers' hostnames out of those logs.
- Apps run with `no-new-privileges`, a process limit and your memory and CPU limits. They share one Docker network, so an app can reach the others: use a firewall inside your apps, or one Apperator per trust boundary, if that matters to you.

## Status

Apperator is new. The API may change before 1.0. It runs on one host. Spreading apps across several hosts is up to the caller for now: run one Apperator per host and keep track of which app lives where.

## Development

```bash
go test ./...                              # unit tests
APPERATOR_DOCKER_TESTS=1 go test ./...      # also run an app through its life on real Docker
go build -o apperator ./cmd/apperator
```

## Licence

Apache-2.0. Copyright 2026 [Automaze](https://github.com/automazeio). Apperator runs Traefik (MIT) and Sablier (AGPL-3.0) as separate, unchanged programs.
