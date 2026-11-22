// Package engine turns app specs into running containers and keeps them that way.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/automazeio/apperator/internal/config"
	"github.com/automazeio/apperator/internal/docker"
	"github.com/automazeio/apperator/internal/store"
	"github.com/automazeio/apperator/internal/webhook"
)

// Engine is safe for concurrent use. Calls for one app run one at a time.
type Engine struct {
	cfg      *config.Config
	docker   *docker.Client
	store    *store.Store
	hooks    *webhook.Sender
	log      *slog.Logger
	http     *http.Client
	starting chan struct{}

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex

	wakeMu sync.Mutex
	wakes  map[string]*wakeState
}

type wakeState struct {
	finishedAt  string    // the stop this state was read after
	at          time.Time // from the wake file; zero when none
	lastAttempt time.Time
}

// New builds an engine. Call Init before using it.
func New(cfg *config.Config, d *docker.Client, s *store.Store, hooks *webhook.Sender, log *slog.Logger) *Engine {
	return &Engine{
		cfg: cfg, docker: d, store: s, hooks: hooks, log: log,
		http: &http.Client{
			Timeout:       2 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		starting: make(chan struct{}, cfg.MaxStarting),
		locks:    map[string]*sync.Mutex{},
		wakes:    map[string]*wakeState{},
	}
}

// Init checks Docker answers and creates the network.
func (e *Engine) Init(ctx context.Context) error {
	if err := e.docker.Ping(ctx); err != nil {
		return fmt.Errorf("cannot reach Docker: %w", err)
	}
	return e.docker.EnsureNetwork(ctx, e.cfg.Network, map[string]string{labelManaged: "true"})
}

// App is an app's desired state and what Docker reports about it.
type App struct {
	ID         string         `json:"id"`
	Spec       Spec           `json:"spec"`
	State      string         `json:"state"`
	Held       bool           `json:"held"`
	Container  *ContainerInfo `json:"container,omitempty"`
	NextWakeAt *time.Time     `json:"next_wake_at,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// ContainerInfo describes the app's current container.
type ContainerInfo struct {
	ID         string `json:"id"`
	ImageID    string `json:"image_id,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	ExitCode   int    `json:"exit_code"`
	OOMKilled  bool   `json:"oom_killed,omitempty"`
	UpToDate   bool   `json:"up_to_date"`
}

// States an app can be in.
const (
	StateRunning    = "running"
	StateStopped    = "stopped"
	StateRestarting = "restarting"
	StatePaused     = "paused"
	StateMissing    = "missing" // no container: it was removed outside Apperator, or creating it failed
)

// Errors the API maps to status codes.
var ErrNotFound = store.ErrNotFound

// ConflictError means the request can't be done in the app's current state.
type ConflictError struct{ Message string }

func (e *ConflictError) Error() string { return e.Message }

// ImageError means the image couldn't be pulled or found.
type ImageError struct{ Err error }

func (e *ImageError) Error() string { return e.Err.Error() }

// StartError means the app didn't start or didn't become healthy. Logs holds its last output.
type StartError struct {
	Message string
	Logs    string
}

func (e *StartError) Error() string { return e.Message }

func (e *Engine) lock(id string) func() {
	e.locksMu.Lock()
	m, ok := e.locks[id]
	if !ok {
		m = &sync.Mutex{}
		e.locks[id] = m
	}
	e.locksMu.Unlock()
	m.Lock()
	return m.Unlock
}

func (e *Engine) name(id string) string { return e.cfg.Prefix + id }

// Apply creates the app or changes it to match spec, starts it and waits until it is healthy.
// It reports whether the app is new.
func (e *Engine) Apply(ctx context.Context, id string, spec Spec) (*App, bool, error) {
	if !ValidID(id) {
		return nil, false, &InvalidError{Problems: []string{"id must be 1 to 63 lowercase letters, digits or hyphens, not starting or ending with a hyphen"}}
	}
	if err := spec.Validate(e.cfg); err != nil {
		return nil, false, err
	}
	defer e.lock(id)()

	rec, err := e.store.Get(id)
	isNew := errors.Is(err, store.ErrNotFound)
	imageID, err := e.ensureImage(ctx, spec.Image, spec.Pull)
	if err != nil {
		return nil, false, err
	}
	raw, _ := json.Marshal(spec)
	if err := e.store.Put(store.Record{ID: id, Spec: raw}); err != nil {
		return nil, false, err
	}
	changed, err := e.ensureContainer(ctx, id, &spec, imageID)
	if err == nil {
		err = e.start(ctx, id, &spec)
	}
	if err != nil {
		e.hooks.Send("app.failed", id, map[string]any{"error": err.Error()})
		return nil, isNew, err
	}
	switch {
	case isNew:
		e.hooks.Send("app.created", id, nil)
	case changed || rec.Held:
		e.hooks.Send("app.updated", id, nil)
	}
	app, err := e.get(ctx, id)
	return app, isNew, err
}

// ensureImage returns the local image ID, pulling the image when it is missing or pull is "always".
func (e *Engine) ensureImage(ctx context.Context, ref, pull string) (string, error) {
	if pull != "always" {
		if id, err := e.docker.ImageID(ctx, ref); err == nil {
			return id, nil
		} else if !docker.IsNotFound(err) {
			return "", err
		}
	}
	if err := e.docker.ImagePull(ctx, ref, e.cfg.RegistryAuth); err != nil {
		return "", &ImageError{Err: err}
	}
	id, err := e.docker.ImageID(ctx, ref)
	if err != nil {
		return "", &ImageError{Err: err}
	}
	return id, nil
}

// ensureContainer creates the container, or recreates it when the spec or image changed. The data
// volume is kept. It reports whether an existing container was replaced.
func (e *Engine) ensureContainer(ctx context.Context, id string, spec *Spec, imageID string) (bool, error) {
	body, hash := containerBody(e.cfg, id, spec, imageID)
	if spec.DataPath != "" {
		if err := e.docker.EnsureVolume(ctx, volumeName(e.cfg, id), map[string]string{labelManaged: "true", labelApp: id}); err != nil {
			return false, err
		}
	}
	name := e.name(id)
	ctr, err := e.docker.Inspect(ctx, name)
	replaced := false
	switch {
	case docker.IsNotFound(err):
	case err != nil:
		return false, err
	case ctr.Config.Labels[labelHash] == hash:
		return false, nil
	default:
		// Stop gracefully first, so the app can finish writes before it is removed.
		if ctr.State.Running {
			if err := e.docker.Stop(ctx, ctr.ID, e.cfg.StopTimeout); err != nil && !docker.IsNotFound(err) {
				return false, err
			}
		}
		if err := e.docker.Remove(ctx, ctr.ID); err != nil {
			return false, err
		}
		replaced = true
	}
	if _, err := e.docker.Create(ctx, name, body); err != nil {
		return replaced, err
	}
	return replaced, nil
}

// start starts the app, through Sablier when it has an idle timeout so the session (and the idle
// stop) is Sablier's, then waits until it is healthy.
func (e *Engine) start(ctx context.Context, id string, spec *Spec) error {
	select {
	case e.starting <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-e.starting }()

	ctx, cancel := context.WithTimeout(ctx, e.cfg.StartTimeout)
	defer cancel()
	name := e.name(id)
	var err error
	if spec.IdleTimeout != "" {
		err = e.sablierStart(ctx, name, spec.IdleTimeout)
	} else {
		err = e.docker.Start(ctx, name)
	}
	if err != nil {
		return &StartError{Message: "start failed: " + err.Error(), Logs: e.tail(name)}
	}
	return e.waitHealthy(ctx, name, spec)
}

func (e *Engine) sablierStart(ctx context.Context, name, session string) error {
	q := url.Values{"names": {name}, "session_duration": {session}, "timeout": {e.cfg.StartTimeout.String()}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.cfg.SablierURL+"/api/strategies/blocking?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("sablier: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("sablier: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func (e *Engine) waitHealthy(ctx context.Context, name string, spec *Spec) error {
	var last string
	for {
		ctr, err := e.docker.Inspect(ctx, name)
		switch {
		case err != nil:
			last = err.Error()
		case ctr.State.Status == "exited" || ctr.State.Status == "dead" || ctr.State.Restarting:
			return &StartError{Message: fmt.Sprintf("the app exited with code %d", ctr.State.ExitCode), Logs: e.tail(name)}
		case ctr.State.Running && spec.HealthPath == "":
			// Without a health check, an app counts as started once it has kept running for a second.
			started, err := time.Parse(time.RFC3339Nano, ctr.State.StartedAt)
			if err == nil && time.Since(started) >= time.Second {
				return nil
			}
			last = "just started"
		case ctr.State.Running:
			ip := ctr.NetworkSettings.Networks[e.cfg.Network].IPAddress
			if ip == "" {
				last = "no IP address on network " + e.cfg.Network
				break
			}
			u := fmt.Sprintf("http://%s:%d%s", ip, spec.Port, spec.HealthPath)
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			res, err := e.http.Do(req)
			if err == nil {
				res.Body.Close()
				if res.StatusCode < 400 {
					return nil
				}
				last = fmt.Sprintf("%s answered HTTP %d", spec.HealthPath, res.StatusCode)
			} else {
				last = err.Error()
			}
		default:
			last = "state " + ctr.State.Status
		}
		select {
		case <-ctx.Done():
			return &StartError{Message: "the app wasn't healthy in time: " + last, Logs: e.tail(name)}
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// tail returns the container's last log lines, for error messages.
func (e *Engine) tail(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logs, _ := e.docker.Logs(ctx, name, 30)
	return logs
}

// Get returns one app.
func (e *Engine) Get(ctx context.Context, id string) (*App, error) { return e.get(ctx, id) }

func (e *Engine) get(ctx context.Context, id string) (*App, error) {
	rec, err := e.store.Get(id)
	if err != nil {
		return nil, err
	}
	app := e.fromRecord(rec)
	ctr, err := e.docker.Inspect(ctx, e.name(id))
	if docker.IsNotFound(err) {
		app.State = StateMissing
		return app, nil
	}
	if err != nil {
		return nil, err
	}
	app.State = stateOf(ctr.State.Status)
	app.Container = &ContainerInfo{
		ID: ctr.ID[:12], ImageID: ctr.Image, StartedAt: ctr.State.StartedAt, FinishedAt: ctr.State.FinishedAt,
		ExitCode: ctr.State.ExitCode, OOMKilled: ctr.State.OOMKilled,
	}
	if imageID, err := e.docker.ImageID(ctx, app.Spec.Image); err == nil {
		_, hash := containerBody(e.cfg, id, &app.Spec, imageID)
		app.Container.UpToDate = ctr.Config.Labels[labelHash] == hash
	}
	e.addWake(app)
	return app, nil
}

// List returns every app.
func (e *Engine) List(ctx context.Context) ([]*App, error) {
	containers, err := e.docker.List(ctx, labelManaged+"=true")
	if err != nil {
		return nil, err
	}
	byApp := map[string]docker.Summary{}
	for _, c := range containers {
		if app := c.Labels[labelApp]; app != "" {
			byApp[app] = c
		}
	}
	out := []*App{}
	for _, rec := range e.store.List() {
		app := e.fromRecord(rec)
		if c, ok := byApp[rec.ID]; ok {
			app.State = stateOf(c.State)
			app.Container = &ContainerInfo{ID: c.ID[:12]}
		} else {
			app.State = StateMissing
		}
		e.addWake(app)
		out = append(out, app)
	}
	return out, nil
}

func (e *Engine) fromRecord(rec store.Record) *App {
	app := &App{ID: rec.ID, Held: rec.Held, CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt}
	_ = json.Unmarshal(rec.Spec, &app.Spec)
	return app
}

func (e *Engine) addWake(app *App) {
	if app.State != StateStopped || app.Held {
		return
	}
	e.wakeMu.Lock()
	defer e.wakeMu.Unlock()
	if w := e.wakes[app.ID]; w != nil && !w.at.IsZero() {
		t := w.at
		app.NextWakeAt = &t
	}
}

func stateOf(docker string) string {
	switch docker {
	case "running":
		return StateRunning
	case "restarting":
		return StateRestarting
	case "paused":
		return StatePaused
	default:
		return StateStopped
	}
}

// Start starts a stopped app and clears its hold, recreating the container if it is missing or
// out of date.
func (e *Engine) Start(ctx context.Context, id string) (*App, error) {
	defer e.lock(id)()
	rec, err := e.store.Get(id)
	if err != nil {
		return nil, err
	}
	app := e.fromRecord(rec)
	if err := e.store.SetHeld(id, false); err != nil {
		return nil, err
	}
	imageID, err := e.ensureImage(ctx, app.Spec.Image, "missing")
	if err == nil {
		_, err = e.ensureContainer(ctx, id, &app.Spec, imageID)
	}
	if err == nil {
		err = e.start(ctx, id, &app.Spec)
	}
	if err != nil {
		e.hooks.Send("app.failed", id, map[string]any{"error": err.Error()})
		return nil, err
	}
	return e.get(ctx, id)
}

// Stop stops the app and holds it: scheduled wake-ups skip it until it is started again. A
// request through the proxy still starts an app with an idle timeout.
func (e *Engine) Stop(ctx context.Context, id string) (*App, error) {
	defer e.lock(id)()
	if err := e.store.SetHeld(id, true); err != nil {
		return nil, err
	}
	if err := e.docker.Stop(ctx, e.name(id), e.cfg.StopTimeout); err != nil && !docker.IsNotFound(err) {
		return nil, err
	}
	return e.get(ctx, id)
}

// Restart stops the app and starts it again.
func (e *Engine) Restart(ctx context.Context, id string) (*App, error) {
	if _, err := e.Stop(ctx, id); err != nil {
		return nil, err
	}
	return e.Start(ctx, id)
}

// ExecRequest is a one-off command to run in an app.
type ExecRequest struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	User    string            `json:"user,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
}

// Exec runs a command in the app, starting it first if it is asleep.
func (e *Engine) Exec(ctx context.Context, id string, req ExecRequest) (*docker.ExecResult, error) {
	if len(req.Command) == 0 {
		return nil, &InvalidError{Problems: []string{"command is required"}}
	}
	timeout := 5 * time.Minute
	if req.Timeout != "" {
		d, err := time.ParseDuration(req.Timeout)
		if err != nil || d <= 0 || d > time.Hour {
			return nil, &InvalidError{Problems: []string{"timeout must be a duration up to 1h"}}
		}
		timeout = d
	}
	env := make([]string, 0, len(req.Env))
	for k, v := range req.Env {
		if !envKeyPattern.MatchString(k) {
			return nil, &InvalidError{Problems: []string{fmt.Sprintf("env: %q is not a valid variable name", k)}}
		}
		env = append(env, k+"="+v)
	}
	sort.Strings(env)

	if err := e.ensureRunning(ctx, id); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return e.docker.Exec(ctx, e.name(id), req.Command, env, req.User, 1<<20)
}

func (e *Engine) ensureRunning(ctx context.Context, id string) error {
	defer e.lock(id)()
	rec, err := e.store.Get(id)
	if err != nil {
		return err
	}
	ctr, err := e.docker.Inspect(ctx, e.name(id))
	if docker.IsNotFound(err) {
		return &ConflictError{Message: "the app has no container: start it first"}
	}
	if err != nil {
		return err
	}
	if ctr.State.Running {
		return nil
	}
	if rec.Held {
		return &ConflictError{Message: "the app was stopped through the API: start it first"}
	}
	app := e.fromRecord(rec)
	return e.start(ctx, id, &app.Spec)
}

// Logs returns the app's last output lines.
func (e *Engine) Logs(ctx context.Context, id string, tail int) (string, error) {
	if _, err := e.store.Get(id); err != nil {
		return "", err
	}
	logs, err := e.docker.Logs(ctx, e.name(id), tail)
	if docker.IsNotFound(err) {
		return "", &ConflictError{Message: "the app has no container"}
	}
	return logs, err
}

// Delete removes the app and its container. The data volume is kept: delete it with
// DeleteVolume.
func (e *Engine) Delete(ctx context.Context, id string) error {
	defer e.lock(id)()
	if _, err := e.store.Get(id); err != nil {
		return err
	}
	name := e.name(id)
	if err := e.docker.Stop(ctx, name, e.cfg.StopTimeout); err != nil && !docker.IsNotFound(err) {
		return err
	}
	if err := e.docker.Remove(ctx, name); err != nil {
		return err
	}
	if err := e.store.Delete(id); err != nil {
		return err
	}
	e.wakeMu.Lock()
	delete(e.wakes, id)
	e.wakeMu.Unlock()
	e.hooks.Send("app.deleted", id, nil)
	return nil
}

// DeleteVolume deletes an app's data for good. The app must be deleted first.
func (e *Engine) DeleteVolume(ctx context.Context, id string) error {
	if !ValidID(id) {
		return ErrNotFound
	}
	defer e.lock(id)()
	if _, err := e.store.Get(id); err == nil {
		return &ConflictError{Message: "delete the app first: DELETE /v1/apps/" + id}
	}
	vol := volumeName(e.cfg, id)
	ok, err := e.docker.VolumeExists(ctx, vol)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if err := e.docker.RemoveVolume(ctx, vol); err != nil {
		if docker.IsConflict(err) {
			return &ConflictError{Message: "the volume is still in use"}
		}
		return err
	}
	e.hooks.Send("app.volume_deleted", id, nil)
	return nil
}
