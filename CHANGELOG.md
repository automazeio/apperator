# Changelog

All notable changes to Apperator are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- The first version: a REST API to create, change, start, stop, restart and delete apps, run commands in them and read their logs.
- Traefik routing and Sablier idle stops through container labels, with a short `sablier.ready-after` delay so a woken app answers the first request.
- Scheduled wake-ups from a wake file in the app's data, and a daily safety-net wake-up.
- Named data volumes that survive recreates and deletes, with a separate call to delete them.
- Signed webhooks for app events.
- A Docker Compose stack with Traefik, Sablier and Apperator.
- Apps keep their image's health check, but Docker runs it every 250 ms while the app starts, instead of after 5 s. The Compose stack also has Sablier check a waking app every 250 ms instead of every 5 s. Together this cut a real app's cold wake from 11.5 s to about 2.7 s.
