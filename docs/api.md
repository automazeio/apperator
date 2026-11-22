# API

Every request except `GET /health` needs the header `Authorization: Bearer <APPERATOR_TOKEN>`. Requests and responses are JSON.

| Method and path | Does |
|---|---|
| `GET /health` | Checks Apperator is up. No token needed. |
| `GET /v1/apps` | Lists every app. |
| `GET /v1/apps/{id}` | Returns one app. |
| `PUT /v1/apps/{id}` | Creates the app, or changes it to match. Waits until it is healthy. |
| `POST /v1/apps/{id}/start` | Starts the app and clears a hold. |
| `POST /v1/apps/{id}/stop` | Stops the app and holds it. |
| `POST /v1/apps/{id}/restart` | Stops and starts the app. |
| `POST /v1/apps/{id}/exec` | Runs a one-off command in the app. |
| `GET /v1/apps/{id}/logs?tail=100` | Returns the app's last output lines, as plain text. |
| `DELETE /v1/apps/{id}` | Deletes the app and its container. Keeps its data. |
| `DELETE /v1/apps/{id}/volume` | Deletes the data of a deleted app, for good. |

## App IDs

An ID is 1 to 63 lowercase letters, digits and hyphens, and doesn't start or end with a hyphen: a valid DNS label. App `acme` runs in the container `apperator-acme`, with its data in the volume `apperator-acme-data` (the prefix is `APPERATOR_PREFIX`).

## The spec

The body of `PUT /v1/apps/{id}`. Only `image` is required.

| Field | Example | Description |
|---|---|---|
| `image` | `"ghcr.io/example/app:1.4"` | The image to run. |
| `pull` | `"missing"` | `missing` (default) pulls the image only when it isn't on the server. `always` pulls on every `PUT`, and recreates the container if the tag now points to a new image. |
| `env` | `{"PUBLIC_URL": "…"}` | Environment variables. |
| `command` | `["node", "server.js"]` | Replaces the image's command. |
| `hostnames` | `["acme.example.com"]` | Hostnames Traefik routes to the app. Needs `port`. |
| `port` | `3000` | The port the app listens on. |
| `health_path` | `"/health"` | Apperator waits until a `GET` on this path answers with a status below 400. Without it, an app counts as started once it has run for a second. |
| `data_path` | `"/data"` | Where the app's volume is mounted. The volume survives recreates and deletes. |
| `memory_mb` | `384` | Memory limit, with no swap. |
| `cpus` | `0.5` | CPU limit, in cores. |
| `idle_timeout` | `"15m"` | Stop the app after this long without requests, and start it on the next one. At least `10s`. Needs `hostnames` and `APPERATOR_SABLIER_URL`. |
| `wake_file` | `"/data/next-work-at"` | A file inside `data_path` where the app writes the next time it needs to run, as an RFC 3339 time, or leaves it empty. Needs `idle_timeout`. |
| `labels` | `{"plan": "pro"}` | Your own notes about the app. Apperator stores and returns them, and does nothing else with them. Changing them doesn't recreate the container. |

Docker fixes a container's settings when it is created. So when anything except `labels` or `pull` changes, Apperator stops the container (giving the app `APPERATOR_STOP_TIMEOUT` seconds to finish), removes it and creates a new one with the same volume.

`PUT` returns `201 Created` for a new app and `200 OK` otherwise, with the app in the body. If the app doesn't start or doesn't become healthy, it returns `502` with the app's last log lines. The new spec is kept either way, so you can fix it and `PUT` again.

## The app

```json
{
  "id": "acme",
  "spec": { "image": "ghcr.io/example/app:1.4", "...": "..." },
  "state": "stopped",
  "held": false,
  "container": {
    "id": "37667fc2059c",
    "image_id": "sha256:a7dbc3…",
    "started_at": "2026-11-22T17:55:15.633Z",
    "finished_at": "2026-11-22T18:10:33.012Z",
    "exit_code": 0,
    "up_to_date": true
  },
  "next_wake_at": "2026-11-23T09:00:00Z",
  "created_at": "2026-11-22T17:55:15.580Z",
  "updated_at": "2026-11-22T17:55:15.580Z"
}
```

- `state` is `running`, `stopped`, `restarting`, `paused`, or `missing` (there is no container: creating it failed, or someone removed it outside Apperator; `start` creates it again).
- `held` is true after `POST /stop`. Scheduled wake-ups skip a held app. A request through Traefik still starts an app with an idle timeout.
- `container.up_to_date` is false when the container no longer matches the spec, for example after a new image was pulled behind the same tag. `start` or `PUT` recreates it.
- `next_wake_at` is when a sleeping app will be woken, from its wake file. It is read once after each stop.
- `GET /v1/apps` returns `{"apps": [...]}` with a shorter `container` (only its ID).

The spec includes the environment variables, which may hold secrets. Anyone with the token can read them.

## Exec

```json
{ "command": ["node", "cli.js", "setup", "--email", "owner@example.com"], "env": {"DEBUG": "1"}, "user": "", "timeout": "5m" }
```

Returns `{"exit_code": 0, "stdout": "…", "stderr": "…"}`, each stream capped at 1 MB. A sleeping app is started first. A held app returns `409`: start it first. `timeout` is at most `1h`, `5m` by default.

## Errors

```json
{ "error": { "code": "invalid_request", "message": "the request is not valid", "problems": ["port is required with hostnames"] } }
```

| Status | Code | When |
|---|---|---|
| 400 | `invalid_request` | The body or the ID is not valid. `problems` lists everything wrong. |
| 401 | `unauthorized` | No token, or the wrong one. |
| 404 | `not_found` | No such app, volume or route. |
| 409 | `conflict` | Not possible in the app's current state, for example deleting the volume of an app that still exists. |
| 422 | `image_unavailable` | The image couldn't be pulled. |
| 502 | `start_failed` | The app exited or wasn't healthy in time. `logs` holds its last output. |
| 500 | `internal` | Anything else, usually Docker. |

## Webhooks

With `APPERATOR_WEBHOOK_URL` set, Apperator posts events there:

```json
{ "id": "evt_8f2a…", "type": "app.stopped", "app": "acme", "at": "2026-11-22T18:10:33Z", "data": { "exit_code": 0 } }
```

| Type | When |
|---|---|
| `app.created` | A `PUT` created the app and it is healthy. |
| `app.updated` | A `PUT` recreated the app, or cleared its hold, and it is healthy. |
| `app.started` | The container started, for any reason. |
| `app.stopped` | The container stopped, for any reason. `data.exit_code` says how. |
| `app.oom` | The app ran out of memory. |
| `app.failed` | A `PUT`, `start` or scheduled wake-up failed. `data.error` says why. |
| `app.deleted` | The app was deleted. |
| `app.volume_deleted` | The app's data was deleted. |

Each request has the header `Apperator-Signature: t=<unix seconds>,v1=<hex>`, where the hex is HMAC-SHA256 of `<t>.<body>` with `APPERATOR_WEBHOOK_SECRET`. Check it, and reject old timestamps.

Delivery is tried 4 times over about 40 seconds, then dropped. Events are not stored: use them to react quickly, and `GET /v1/apps` as the source of truth.
