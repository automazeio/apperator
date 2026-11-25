// Command apperator runs apps in containers, behind a REST API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/automazeio/apperator/internal/api"
	"github.com/automazeio/apperator/internal/config"
	"github.com/automazeio/apperator/internal/docker"
	"github.com/automazeio/apperator/internal/engine"
	"github.com/automazeio/apperator/internal/store"
	"github.com/automazeio/apperator/internal/webhook"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const usage = `Apperator runs apps in containers, behind a REST API.

Usage:
  apperator serve     Start the API, the scheduler and the event watcher
  apperator check     Check the settings, Docker and the folders, then exit (run it before a deploy)
  apperator version   Print the version
  apperator probe URL Check URL answers below HTTP 400 (the health check inside apps)

Settings come from environment variables. See https://github.com/automazeio/apperator.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := serve(); err != nil {
			fmt.Fprintln(os.Stderr, "apperator:", err)
			os.Exit(1)
		}
	case "check":
		if err := check(); err != nil {
			fmt.Fprintln(os.Stderr, "apperator:", err)
			os.Exit(1)
		}
		fmt.Println("ok")
	case "probe":
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: apperator probe URL")
			os.Exit(2)
		}
		os.Exit(engine.Probe(os.Args[2]))
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "apperator: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func serve() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	}
	log := slog.New(handler)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := docker.New(cfg.DockerHost)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	hooks := webhook.New(cfg.WebhookURL, cfg.WebhookSecret, log)
	eng := engine.New(cfg, d, st, hooks, log)
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = eng.Init(initCtx)
	cancel()
	if err != nil {
		return err
	}

	go supervise(ctx, log, "webhooks", hooks.Run)
	go supervise(ctx, log, "docker events", eng.WatchEvents)
	go supervise(ctx, log, "scheduler", eng.RunScheduler)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.Handler(eng, cfg.Token, version, log),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("apperator started", "version", version, "listen", cfg.Listen, "network", cfg.Network, "router", cfg.Router, "sablier", cfg.SablierURL != "")

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("apperator stopping")
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// supervise runs a background loop until ctx ends. A panic in it is logged and the loop starts
// again a second later, instead of ending the process: while Apperator is down, nobody can change
// apps.
func supervise(ctx context.Context, log *slog.Logger, name string, run func(context.Context)) {
	for ctx.Err() == nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("recovered from a panic", "in", name, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				}
			}()
			run(ctx)
		}()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// check tests what serve needs, without starting it, so a deploy can stop before a bad change:
//
//	docker compose run --rm --no-deps apperator check
func check() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := docker.New(cfg.DockerHost)
	if err != nil {
		return err
	}
	if err := d.Ping(ctx); err != nil {
		return fmt.Errorf("cannot reach Docker: %w", err)
	}
	if _, err := store.Open(cfg.DataDir); err != nil {
		return err
	}
	if err := engine.CheckWritable(cfg.DataDir); err != nil {
		return fmt.Errorf("APPERATOR_DATA_DIR: %w", err)
	}
	if cfg.Router == "traefik" {
		if err := engine.CheckWritable(filepath.Dir(cfg.RoutesFile)); err != nil {
			return fmt.Errorf("APPERATOR_ROUTES_FILE: %w", err)
		}
	}
	if cfg.ProbeBinary != "" {
		if _, err := engine.LoadProbe(cfg.ProbeBinary); err != nil {
			fmt.Fprintln(os.Stderr, "warning: no health probe:", err)
		}
	}
	return nil
}
