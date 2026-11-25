// Package config reads Apperator's settings from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every setting. Each field names its environment variable.
type Config struct {
	Token      string // APPERATOR_TOKEN: bearer token for the API (required, 32+ characters)
	Listen     string // APPERATOR_LISTEN: API address, default ":7070"
	DataDir    string // APPERATOR_DATA_DIR: where app specs are kept, default "/var/lib/apperator"
	DockerHost string // DOCKER_HOST: default unix:///var/run/docker.sock
	Network    string // APPERATOR_NETWORK: Docker network for apps, the proxy and Sablier, default "apperator"
	Prefix     string // APPERATOR_PREFIX: container and volume name prefix, default "apperator-"

	Router        string        // APPERATOR_ROUTER: "traefik" (routes written to RoutesFile, default) or "none"
	RoutesFile    string        // APPERATOR_ROUTES_FILE: Traefik's routes, for its file provider, default "/routes/apperator.yml"
	Entrypoints   string        // APPERATOR_ENTRYPOINTS: Traefik entrypoints, default "websecure"
	TLS           bool          // APPERATOR_TLS: mark routes as TLS, default true
	CertResolver  string        // APPERATOR_CERT_RESOLVER: Traefik certificate resolver, optional
	SablierURL    string        // APPERATOR_SABLIER_URL: for example "http://sablier:10000"; enables idle_timeout
	BlockingLimit time.Duration // APPERATOR_SABLIER_TIMEOUT: how long a request waits for a sleeping app, default 30s

	StartTimeout time.Duration // APPERATOR_START_TIMEOUT: how long a start may take to be healthy, default 60s
	StopTimeout  int           // APPERATOR_STOP_TIMEOUT: seconds before a stopping app is killed, default 20
	MaxStarting  int           // APPERATOR_MAX_STARTING: apps starting at once, default 4
	MaxRunning   int           // APPERATOR_MAX_RUNNING: scheduled wake-ups wait while this many apps run, 0 = no cap
	SafetyWake   time.Duration // APPERATOR_SAFETY_WAKE: wake sleeping apps this long after they stopped, default 24h, 0 = off
	Tick         time.Duration // APPERATOR_TICK: how often wake-ups are checked, default 30s

	WebhookURL    string // APPERATOR_WEBHOOK_URL: where events are posted, optional
	WebhookSecret string // APPERATOR_WEBHOOK_SECRET: signs events, required with a webhook URL

	ProbeBinary  string // APPERATOR_PROBE_BINARY: a static Linux build of Apperator, copied into apps as their health probe; default this binary, "off" to turn it off
	RegistryAuth string // APPERATOR_REGISTRY_AUTH: X-Registry-Auth value for private images, optional
	LogFormat    string // APPERATOR_LOG_FORMAT: "text" (default) or "json"
}

// FromEnv reads the configuration and checks it.
func FromEnv() (*Config, error) {
	c := &Config{
		Token:         os.Getenv("APPERATOR_TOKEN"),
		Listen:        env("APPERATOR_LISTEN", ":7070"),
		DataDir:       env("APPERATOR_DATA_DIR", "/var/lib/apperator"),
		DockerHost:    os.Getenv("DOCKER_HOST"),
		Network:       env("APPERATOR_NETWORK", "apperator"),
		Prefix:        env("APPERATOR_PREFIX", "apperator-"),
		Router:        env("APPERATOR_ROUTER", "traefik"),
		RoutesFile:    env("APPERATOR_ROUTES_FILE", "/routes/apperator.yml"),
		Entrypoints:   env("APPERATOR_ENTRYPOINTS", "websecure"),
		CertResolver:  os.Getenv("APPERATOR_CERT_RESOLVER"),
		SablierURL:    strings.TrimRight(os.Getenv("APPERATOR_SABLIER_URL"), "/"),
		WebhookURL:    os.Getenv("APPERATOR_WEBHOOK_URL"),
		WebhookSecret: os.Getenv("APPERATOR_WEBHOOK_SECRET"),
		ProbeBinary:   os.Getenv("APPERATOR_PROBE_BINARY"),
		RegistryAuth:  os.Getenv("APPERATOR_REGISTRY_AUTH"),
		LogFormat:     env("APPERATOR_LOG_FORMAT", "text"),
	}
	switch c.ProbeBinary {
	case "off":
		c.ProbeBinary = ""
	case "":
		c.ProbeBinary, _ = os.Executable()
	}
	var errs []string
	add := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	var err error
	c.TLS, err = boolEnv("APPERATOR_TLS", true)
	add(err)
	c.StartTimeout, err = durationEnv("APPERATOR_START_TIMEOUT", 60*time.Second)
	add(err)
	c.BlockingLimit, err = durationEnv("APPERATOR_SABLIER_TIMEOUT", 30*time.Second)
	add(err)
	c.SafetyWake, err = durationEnv("APPERATOR_SAFETY_WAKE", 24*time.Hour)
	add(err)
	c.Tick, err = durationEnv("APPERATOR_TICK", 30*time.Second)
	add(err)
	c.StopTimeout, err = intEnv("APPERATOR_STOP_TIMEOUT", 20)
	add(err)
	c.MaxStarting, err = intEnv("APPERATOR_MAX_STARTING", 4)
	add(err)
	c.MaxRunning, err = intEnv("APPERATOR_MAX_RUNNING", 0)
	add(err)

	if len(c.Token) < 32 {
		errs = append(errs, "APPERATOR_TOKEN must be set, with at least 32 characters (try: openssl rand -hex 32)")
	}
	if c.Router != "traefik" && c.Router != "none" {
		errs = append(errs, `APPERATOR_ROUTER must be "traefik" or "none"`)
	}
	if c.Router == "traefik" && !strings.HasSuffix(c.RoutesFile, ".yml") && !strings.HasSuffix(c.RoutesFile, ".yaml") {
		errs = append(errs, "APPERATOR_ROUTES_FILE must end in .yml or .yaml, or Traefik ignores it")
	}
	if c.WebhookURL != "" && c.WebhookSecret == "" {
		errs = append(errs, "APPERATOR_WEBHOOK_SECRET is required with APPERATOR_WEBHOOK_URL")
	}
	if c.MaxStarting < 1 {
		errs = append(errs, "APPERATOR_MAX_STARTING must be 1 or more")
	}
	if c.Tick < time.Second {
		errs = append(errs, "APPERATOR_TICK must be at least 1s")
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		errs = append(errs, `APPERATOR_LOG_FORMAT must be "text" or "json"`)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("configuration:\n  %s", strings.Join(errs, "\n  "))
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func boolEnv(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("%s must be true or false", key)
	}
	return b, nil
}

func intEnv(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def, fmt.Errorf("%s must be a whole number", key)
	}
	return n, nil
}

func durationEnv(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return def, fmt.Errorf("%s must be a duration such as 30s or 24h", key)
	}
	return d, nil
}
