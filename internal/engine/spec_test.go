package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/automazeio/apperator/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Network: "apperator", Prefix: "apperator-", Router: "traefik", Entrypoints: "websecure", TLS: true,
		SablierURL: "http://sablier:10000", BlockingLimit: 30 * time.Second, StopTimeout: 20,
	}
}

func validSpec() Spec {
	return Spec{
		Image: "ghcr.io/example/app:1.0", Env: map[string]string{"PUBLIC_URL": "https://acme.example.com"},
		Hostnames: []string{"acme.example.com"}, Port: 3000, HealthPath: "/health", DataPath: "/data",
		MemoryMB: 384, IdleTimeout: "15m", WakeFile: "/data/next-work-at",
	}
}

func TestValidate(t *testing.T) {
	cfg := testConfig()
	s := validSpec()
	if err := s.Validate(cfg); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	cases := map[string]func(*Spec){
		"image is required":                      func(s *Spec) { s.Image = "" },
		"not a valid variable name":              func(s *Spec) { s.Env["1BAD"] = "x" },
		"not a valid hostname":                   func(s *Spec) { s.Hostnames = []string{"https://acme.example.com"} },
		"port is required with hostnames":        func(s *Spec) { s.Port = 0; s.HealthPath = "" },
		"health_path must start with /":          func(s *Spec) { s.HealthPath = "health" },
		"data_path must be an absolute path":     func(s *Spec) { s.DataPath = "/"; s.WakeFile = "" },
		"memory_mb must be at least 6":           func(s *Spec) { s.MemoryMB = 2 },
		"idle_timeout must be a duration":        func(s *Spec) { s.IdleTimeout = "soon" },
		"idle_timeout must be at least 10s":      func(s *Spec) { s.IdleTimeout = "1s" },
		"wake_file must be inside data_path":     func(s *Spec) { s.WakeFile = "/tmp/next" },
		"wake_file only works with idle_timeout": func(s *Spec) { s.IdleTimeout = "" },
		`pull must be "missing" or "always"`:     func(s *Spec) { s.Pull = "sometimes" },
		"not a valid key":                        func(s *Spec) { s.Labels = map[string]string{"Plan": "pro"} },
	}
	for want, change := range cases {
		s := validSpec()
		s.Env = map[string]string{"PUBLIC_URL": "x"}
		change(&s)
		err := s.Validate(cfg)
		var invalid *InvalidError
		if !errors.As(err, &invalid) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}

	noSablier := testConfig()
	noSablier.SablierURL = ""
	s = validSpec()
	if err := s.Validate(noSablier); err == nil || !strings.Contains(err.Error(), "APPERATOR_SABLIER_URL") {
		t.Errorf("idle_timeout without Sablier: got %v", err)
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"acme", "a", "acme-2", strings.Repeat("a", 63)} {
		if !ValidID(id) {
			t.Errorf("%q should be valid", id)
		}
	}
	for _, id := range []string{"", "-acme", "acme-", "Acme", "acme.co", "../x", strings.Repeat("a", 64)} {
		if ValidID(id) {
			t.Errorf("%q should be invalid", id)
		}
	}
}

func TestContainerBodyHash(t *testing.T) {
	cfg := testConfig()
	s := validSpec()
	_, h1 := containerBody(cfg, "acme", &s, "sha256:aaa")
	_, h2 := containerBody(cfg, "acme", &s, "sha256:aaa")
	if h1 != h2 {
		t.Fatal("hash is not stable")
	}
	s.Labels = map[string]string{"plan": "pro"}
	s.Pull = "always"
	if _, h := containerBody(cfg, "acme", &s, "sha256:aaa"); h != h1 {
		t.Error("caller labels and pull policy must not recreate the container")
	}
	s.Env = map[string]string{"PUBLIC_URL": "https://acme.example.com", "ACTIVE": "false"}
	if _, h := containerBody(cfg, "acme", &s, "sha256:aaa"); h == h1 {
		t.Error("an env change must change the hash")
	}
	s = validSpec()
	if _, h := containerBody(cfg, "acme", &s, "sha256:bbb"); h == h1 {
		t.Error("a new image must change the hash")
	}
}

func TestContainerBody(t *testing.T) {
	cfg := testConfig()
	s := validSpec()
	body, hash := containerBody(cfg, "acme", &s, "sha256:aaa")
	labels := body["Labels"].(map[string]string)
	want := map[string]string{
		"apperator.managed": "true",
		"apperator.app":     "acme",
		"apperator.hash":    hash,
		"traefik.enable":    "false",
		"sablier.enable":    "true",
		"sablier.group":     "apperator-acme",
	}
	if len(labels) != len(want) {
		t.Errorf("labels: %v", labels)
	}
	for k, v := range want {
		if labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, labels[k], v)
		}
	}
	host := body["HostConfig"].(map[string]any)
	if host["RestartPolicy"].(map[string]any)["Name"] != "no" {
		t.Error("an app with an idle timeout must not restart on its own")
	}
	if host["Memory"].(int64) != 384<<20 {
		t.Error("memory limit not set")
	}

	s.IdleTimeout, s.WakeFile = "", ""
	body, _ = containerBody(cfg, "acme", &s, "sha256:aaa")
	labels = body["Labels"].(map[string]string)
	if labels["sablier.enable"] != "" || body["HostConfig"].(map[string]any)["RestartPolicy"].(map[string]any)["Name"] != "unless-stopped" {
		t.Error("an always-on app must not use Sablier and must restart on its own")
	}

	s.Hostnames = nil
	body, _ = containerBody(cfg, "acme", &s, "sha256:aaa")
	if body["Labels"].(map[string]string)["sablier.enable"] != "" {
		t.Error("an app without hostnames must not use Sablier")
	}
}

func TestParseWakeTime(t *testing.T) {
	if got := ParseWakeTime("2026-11-23T10:00:00.000Z\n"); !got.Equal(time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("got %v", got)
	}
	for _, s := range []string{"", "\n", "soon", "2026-11-23"} {
		if !ParseWakeTime(s).IsZero() {
			t.Errorf("%q should mean no wake-up", s)
		}
	}
}

func TestTraefikConfig(t *testing.T) {
	cfg := testConfig()
	cfg.CertResolver = "letsencrypt"
	s := validSpec()
	s.Hostnames = []string{"acme.example.com", "sign.acme.com"}

	always := Spec{Image: "x", Hostnames: []string{"b.example.com"}, Port: 8080}
	internal := Spec{Image: "x"}
	got := toJSONString(TraefikConfig(cfg, []string{"acme", "b", "c"}, []*Spec{&s, &always, &internal}))
	for _, want := range []string{
		`"apperator-acme":{"entryPoints":["websecure"],"middlewares":["apperator-acme-sablier"],"rule":"Host(` + "`acme.example.com`) || Host(`sign.acme.com`)" + `","service":"apperator-acme","tls":{"certResolver":"letsencrypt"}}`,
		`"apperator-acme":{"loadBalancer":{"servers":[{"url":"http://apperator-acme:3000"}]}}`,
		`"apperator-acme-sablier":{"plugin":{"sablier":{"blocking":{"timeout":"30s"},"group":"apperator-acme","sablierUrl":"http://sablier:10000","sessionDuration":"15m"}}}`,
		`"apperator-b":{"entryPoints":["websecure"],"rule":"Host(` + "`b.example.com`)" + `","service":"apperator-b","tls":{"certResolver":"letsencrypt"}}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config is missing %s\n%s", want, got)
		}
	}
	if strings.Contains(got, "apperator-c") {
		t.Errorf("an app without hostnames got a route: %s", got)
	}
	if got := toJSONString(TraefikConfig(cfg, nil, nil)); got != `{"http":{}}` {
		t.Errorf("empty config: %s", got)
	}
	cfg.FallbackURL = "https://example.com/"
	got = toJSONString(TraefikConfig(cfg, nil, nil))
	for _, want := range []string{
		`"apperator-fallback":{"entryPoints":["websecure"],"middlewares":["apperator-fallback"],"priority":1,"rule":"HostRegexp(` + "`^.+$`)" + `","service":"noop@internal","tls":{}}`,
		`"apperator-fallback":{"redirectRegex":{"regex":"^.*$","replacement":"https://example.com/"}}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("fallback config is missing %s\n%s", want, got)
		}
	}
	if strings.Contains(got, `"services"`) {
		t.Errorf("the fallback alone needs no services: %s", got)
	}
	cfg.Router = "none"
	if got := toJSONString(TraefikConfig(cfg, []string{"acme"}, []*Spec{&s})); got != `{"http":{}}` {
		t.Errorf("router none must serve no routes: %s", got)
	}
}

func toJSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
