# Changelog

All notable changes to Apperator are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- The first version: a REST API to create, change, start, stop, restart and delete apps, run commands in them and read their logs.
- Traefik routing through Traefik's HTTP provider: Apperator serves the routes at `GET /traefik`, with its own read-only token (`APPERATOR_TRAEFIK_TOKEN`). Routes point at each app's name on the Docker network, so they don't change when an app sleeps or wakes, and a waking request reaches the app without a redirect. Traefik no longer needs the Docker socket.
- Sablier idle stops through container labels.
- Scheduled wake-ups from a wake file in the app's data, and a daily safety-net wake-up.
- Named data volumes that survive recreates and deletes, with a separate call to delete them.
- Signed webhooks for app events.
- A Docker Compose stack with Traefik, Sablier and Apperator.
- Apps keep their image's health check, but Docker runs it every 250 ms while the app starts, instead of after 5 s. The Compose stack also has Sablier check a waking app every 250 ms instead of every 5 s. Together with the fixed routes, this cut a real app's cold wake from 11.5 s to about 2.2 s.
