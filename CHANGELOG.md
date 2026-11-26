# Changelog

All notable changes to Apperator are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- The first version: a REST API to create, change, start, stop, restart and delete apps, run commands in them and read their logs.
- Traefik routing through a routes file (`APPERATOR_ROUTES_FILE`) that Traefik's file provider watches. Routes point at each app's name on the Docker network, so they don't change when an app sleeps or wakes, and a waking request reaches the app without a redirect. The file stays in place while Apperator is down, so apps keep being served and woken. Traefik doesn't need the Docker socket.
- `APPERATOR_FALLBACK_URL` redirects requests for hostnames with no app, such as a deleted app's old address.
- `apperator check` tests the settings, Docker and the folders without starting, to run before a deploy.
- A panic in the scheduler, the Docker event watcher or webhook delivery is logged and the loop starts again, instead of stopping Apperator.
- Sablier idle stops through container labels.
- `health_path` becomes the container's Docker health check, run by Apperator's own small binary copied into the container (`apperator probe`). A request that wakes an app now waits until the app answers, even if its image has no health check (it got HTTP 502 before), and the check starts in milliseconds instead of starting the app's own runtime.
- Scheduled wake-ups from a wake file in the app's data, and a daily safety-net wake-up.
- Named data volumes that survive recreates and deletes, with a separate call to delete them.
- Signed webhooks for app events.
- A Docker Compose stack with Traefik, Sablier and Apperator.
- Apps keep their image's health check, but Docker runs it every 250 ms while the app starts, instead of after 5 s. The Compose stack also has Sablier check a waking app every 250 ms instead of every 5 s. Together with the fixed routes, this cut a real app's cold wake from 11.5 s to about 2.2 s.
