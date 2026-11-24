package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/automazeio/apperator/internal/config"
)

// Spec is the desired state of one app, sent with PUT /v1/apps/{id}.
type Spec struct {
	Image       string            `json:"image"`
	Env         map[string]string `json:"env,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Hostnames   []string          `json:"hostnames,omitempty"`
	Port        int               `json:"port,omitempty"`
	HealthPath  string            `json:"health_path,omitempty"`
	DataPath    string            `json:"data_path,omitempty"`
	MemoryMB    int               `json:"memory_mb,omitempty"`
	CPUs        float64           `json:"cpus,omitempty"`
	IdleTimeout string            `json:"idle_timeout,omitempty"`
	WakeFile    string            `json:"wake_file,omitempty"`
	Pull        string            `json:"pull,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

var (
	idPattern       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	hostnamePattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$|^localhost$|^[a-z0-9-]+\.localhost$`)
	envKeyPattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	labelKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,62}$`)
)

// ValidID reports whether id can name an app: a DNS label, 1 to 63 lowercase letters, digits and
// hyphens.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Validate checks a spec and returns every problem at once.
func (s *Spec) Validate(cfg *config.Config) error {
	var errs []string
	bad := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if strings.TrimSpace(s.Image) == "" {
		bad("image is required")
	}
	for k := range s.Env {
		if !envKeyPattern.MatchString(k) {
			bad("env: %q is not a valid variable name", k)
		}
	}
	for _, h := range s.Hostnames {
		if !hostnamePattern.MatchString(h) || len(h) > 253 {
			bad("hostnames: %q is not a valid hostname (lowercase, no scheme or port)", h)
		}
	}
	if s.Port < 0 || s.Port > 65535 {
		bad("port must be between 1 and 65535")
	}
	if len(s.Hostnames) > 0 && s.Port == 0 {
		bad("port is required with hostnames")
	}
	if s.HealthPath != "" {
		if !strings.HasPrefix(s.HealthPath, "/") {
			bad("health_path must start with /")
		}
		if s.Port == 0 {
			bad("port is required with health_path")
		}
	}
	if s.DataPath != "" && (!path.IsAbs(s.DataPath) || path.Clean(s.DataPath) == "/") {
		bad("data_path must be an absolute path other than /")
	}
	if s.MemoryMB != 0 && s.MemoryMB < 6 {
		bad("memory_mb must be at least 6")
	}
	if s.CPUs < 0 {
		bad("cpus must not be negative")
	}
	if s.IdleTimeout != "" {
		d, err := time.ParseDuration(s.IdleTimeout)
		switch {
		case err != nil:
			bad("idle_timeout must be a duration such as 15m")
		case d < 10*time.Second:
			bad("idle_timeout must be at least 10s")
		case cfg.SablierURL == "":
			bad("idle_timeout needs Sablier: set APPERATOR_SABLIER_URL")
		case cfg.Router == "none" || len(s.Hostnames) == 0:
			bad("idle_timeout needs hostnames and the Traefik router, so requests can wake the app")
		}
	}
	if s.WakeFile != "" {
		if !path.IsAbs(s.WakeFile) {
			bad("wake_file must be an absolute path")
		}
		if s.IdleTimeout == "" {
			bad("wake_file only works with idle_timeout")
		}
		if s.DataPath == "" || !strings.HasPrefix(path.Clean(s.WakeFile), path.Clean(s.DataPath)+"/") {
			bad("wake_file must be inside data_path, so it survives a recreate")
		}
	}
	if s.Pull != "" && s.Pull != "missing" && s.Pull != "always" {
		bad(`pull must be "missing" or "always"`)
	}
	for k := range s.Labels {
		if !labelKeyPattern.MatchString(k) {
			bad("labels: %q is not a valid key (lowercase letters, digits, . _ / -)", k)
		}
	}
	if len(errs) > 0 {
		return &InvalidError{Problems: errs}
	}
	return nil
}

// InvalidError lists what is wrong with a request.
type InvalidError struct{ Problems []string }

func (e *InvalidError) Error() string { return strings.Join(e.Problems, "; ") }

func (s *Spec) idle() time.Duration {
	d, _ := time.ParseDuration(s.IdleTimeout)
	return d
}

// containerBody builds the Docker create body for an app. imageID pins the hash to the exact
// image, so a new image behind the same tag recreates the container when pull is "always".
func containerBody(cfg *config.Config, id string, s *Spec, imageID string) (map[string]any, string) {
	name := cfg.Prefix + id
	labels := map[string]string{labelManaged: "true", labelApp: id}
	for k, v := range routeLabels(cfg, id, s) {
		labels[k] = v
	}

	env := make([]string, 0, len(s.Env))
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)

	restart := "unless-stopped"
	if s.IdleTimeout != "" {
		// Sablier starts it on the next request, so a host reboot doesn't wake every app at once.
		restart = "no"
	}
	host := map[string]any{
		"NetworkMode":   cfg.Network,
		"RestartPolicy": map[string]any{"Name": restart},
		"Init":          true,
		"SecurityOpt":   []string{"no-new-privileges:true"},
		"PidsLimit":     512,
		"LogConfig":     map[string]any{"Type": "local", "Config": map[string]string{"max-size": "5m", "max-file": "2"}},
	}
	if s.MemoryMB > 0 {
		host["Memory"] = int64(s.MemoryMB) << 20
		host["MemorySwap"] = int64(s.MemoryMB) << 20
	}
	if s.CPUs > 0 {
		host["NanoCpus"] = int64(s.CPUs * 1e9)
	}
	if s.DataPath != "" {
		host["Mounts"] = []map[string]any{{"Type": "volume", "Source": volumeName(cfg, id), "Target": s.DataPath}}
	}
	body := map[string]any{
		"Image":       s.Image,
		"Env":         env,
		"Labels":      labels,
		"StopTimeout": cfg.StopTimeout,
		"HostConfig":  host,
		// Keep the image's own health check, but run it every 250ms while the app starts. Docker's
		// default is 5s, and Sablier holds a waking request until the container is healthy.
		"Healthcheck":      map[string]any{"StartInterval": int64(250 * time.Millisecond), "StartPeriod": int64(cfg.StartTimeout)},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{cfg.Network: map[string]any{"Aliases": []string{name}}}},
	}
	if len(s.Command) > 0 {
		body["Cmd"] = s.Command
	}
	if s.Port > 0 {
		body["ExposedPorts"] = map[string]any{fmt.Sprintf("%d/tcp", s.Port): map[string]any{}}
	}

	// encoding/json sorts map keys, so the hash is stable.
	b, _ := json.Marshal(map[string]any{"body": body, "image_id": imageID})
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:12])
	labels[labelHash] = hash
	return body, hash
}

// routeLabels returns the labels Sablier needs to start and stop an app. Traefik's routes come
// from TraefikConfig. "traefik.enable=false" keeps the app private from a Traefik that also
// watches Docker with exposedByDefault on.
func routeLabels(cfg *config.Config, id string, s *Spec) map[string]string {
	l := map[string]string{"traefik.enable": "false"}
	if cfg.Router == "traefik" && len(s.Hostnames) > 0 && s.IdleTimeout != "" && cfg.SablierURL != "" {
		l["sablier.enable"] = "true"
		l["sablier.group"] = cfg.Prefix + id
	}
	return l
}

func hostRule(hostnames []string) string {
	rules := make([]string, len(hostnames))
	for i, h := range hostnames {
		rules[i] = "Host(`" + h + "`)"
	}
	return strings.Join(rules, " || ")
}

// TraefikConfig returns Traefik's dynamic configuration for every routed app, for Traefik's HTTP
// provider. Each service points at the app's name on the Docker network, which never changes, so
// Traefik doesn't reload when an app sleeps or wakes, and a waking request goes straight through.
func TraefikConfig(cfg *config.Config, ids []string, specs []*Spec) map[string]any {
	if cfg.Router != "traefik" {
		return map[string]any{"http": map[string]any{}}
	}
	routers := map[string]any{}
	services := map[string]any{}
	middlewares := map[string]any{}
	for i, id := range ids {
		s := specs[i]
		if len(s.Hostnames) == 0 || s.Port == 0 {
			continue
		}
		r := cfg.Prefix + id
		router := map[string]any{"rule": hostRule(s.Hostnames), "entryPoints": strings.Split(cfg.Entrypoints, ","), "service": r}
		if cfg.TLS {
			tls := map[string]any{}
			if cfg.CertResolver != "" {
				tls["certResolver"] = cfg.CertResolver
			}
			router["tls"] = tls
		}
		if s.IdleTimeout != "" && cfg.SablierURL != "" {
			m := r + "-sablier"
			middlewares[m] = map[string]any{"plugin": map[string]any{"sablier": map[string]any{
				"sablierUrl":      cfg.SablierURL,
				"group":           r,
				"sessionDuration": s.IdleTimeout,
				"blocking":        map[string]any{"timeout": cfg.BlockingLimit.String()},
			}}}
			router["middlewares"] = []string{m}
		}
		routers[r] = router
		services[r] = map[string]any{"loadBalancer": map[string]any{"servers": []map[string]any{{"url": fmt.Sprintf("http://%s:%d", r, s.Port)}}}}
	}
	httpCfg := map[string]any{}
	if len(routers) > 0 {
		httpCfg["routers"] = routers
		httpCfg["services"] = services
	}
	if len(middlewares) > 0 {
		httpCfg["middlewares"] = middlewares
	}
	return map[string]any{"http": httpCfg}
}

func volumeName(cfg *config.Config, id string) string { return cfg.Prefix + id + "-data" }

const (
	labelManaged = "apperator.managed"
	labelApp     = "apperator.app"
	labelHash    = "apperator.hash"
)
