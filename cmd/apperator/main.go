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
  apperator version   Print the version

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

	go hooks.Run(ctx)
	go eng.WatchEvents(ctx)
	go eng.RunScheduler(ctx)

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
