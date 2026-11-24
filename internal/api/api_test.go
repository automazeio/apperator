package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/automazeio/apperator/internal/docker"
	"github.com/automazeio/apperator/internal/engine"
)

const token = "0123456789abcdef0123456789abcdef"

type fake struct{ apps map[string]*engine.App }

func (f *fake) Apply(_ context.Context, id string, s engine.Spec) (*engine.App, bool, error) {
	if s.Image == "" {
		return nil, false, &engine.InvalidError{Problems: []string{"image is required"}}
	}
	if s.Image == "broken" {
		return nil, false, &engine.StartError{Message: "the app exited with code 1", Logs: "boom"}
	}
	_, existed := f.apps[id]
	f.apps[id] = &engine.App{ID: id, Spec: s, State: engine.StateRunning}
	return f.apps[id], !existed, nil
}
func (f *fake) Get(_ context.Context, id string) (*engine.App, error) {
	if a, ok := f.apps[id]; ok {
		return a, nil
	}
	return nil, engine.ErrNotFound
}
func (f *fake) List(context.Context) ([]*engine.App, error) {
	out := []*engine.App{}
	for _, a := range f.apps {
		out = append(out, a)
	}
	return out, nil
}
func (f *fake) Start(ctx context.Context, id string) (*engine.App, error)   { return f.Get(ctx, id) }
func (f *fake) Stop(ctx context.Context, id string) (*engine.App, error)    { return f.Get(ctx, id) }
func (f *fake) Restart(ctx context.Context, id string) (*engine.App, error) { return f.Get(ctx, id) }
func (f *fake) Exec(_ context.Context, id string, r engine.ExecRequest) (*docker.ExecResult, error) {
	return &docker.ExecResult{Stdout: strings.Join(r.Command, " ")}, nil
}
func (f *fake) Logs(context.Context, string, int) (string, error) { return "line\n", nil }
func (f *fake) Delete(_ context.Context, id string) error {
	if _, ok := f.apps[id]; !ok {
		return engine.ErrNotFound
	}
	delete(f.apps, id)
	return nil
}
func (f *fake) DeleteVolume(_ context.Context, id string) error {
	if _, ok := f.apps[id]; ok {
		return &engine.ConflictError{Message: "delete the app first"}
	}
	return nil
}

func (f *fake) TraefikConfig() map[string]any { return map[string]any{"http": map[string]any{}} }

const traefikToken = "traefik-token-traefik-token-traefik-token"

func call(t *testing.T, h http.Handler, method, path, body string, auth bool) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return rec.Code, out
}

func TestAPI(t *testing.T) {
	h := Handler(&fake{apps: map[string]*engine.App{}}, token, traefikToken, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))

	// GET /traefik takes only its own token, and the Traefik token opens nothing else.
	for _, c := range []struct {
		path, auth string
		want       int
	}{
		{"/traefik", traefikToken, 200},
		{"/traefik", token, 404},
		{"/traefik", "", 401},
		{"/v1/apps", traefikToken, 401},
	} {
		req := httptest.NewRequest("GET", c.path, nil)
		if c.auth != "" {
			req.Header.Set("Authorization", "Bearer "+c.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("GET %s with %q: %d, want %d", c.path, c.auth, rec.Code, c.want)
		}
	}

	if code, body := call(t, h, "GET", "/health", "", false); code != 200 || body["version"] != "test" {
		t.Errorf("health: %d %v", code, body)
	}
	if code, _ := call(t, h, "GET", "/v1/apps", "", false); code != 401 {
		t.Errorf("no token: %d", code)
	}
	req := httptest.NewRequest("GET", "/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong token: %d", rec.Code)
	}

	if code, body := call(t, h, "PUT", "/v1/apps/acme", `{"image":"nginx:alpine"}`, true); code != 201 || body["id"] != "acme" {
		t.Errorf("create: %d %v", code, body)
	}
	if code, _ := call(t, h, "PUT", "/v1/apps/acme", `{"image":"nginx:alpine"}`, true); code != 200 {
		t.Errorf("update: %d", code)
	}
	code, body := call(t, h, "PUT", "/v1/apps/acme", `{}`, true)
	if code != 400 || !strings.Contains(toJSON(body), "image is required") {
		t.Errorf("invalid: %d %v", code, body)
	}
	if code, _ := call(t, h, "PUT", "/v1/apps/acme", `{"image":"x","imagee":"y"}`, true); code != 400 {
		t.Errorf("unknown field: %d", code)
	}
	code, body = call(t, h, "PUT", "/v1/apps/acme", `{"image":"broken"}`, true)
	if code != 502 || !strings.Contains(toJSON(body), "boom") {
		t.Errorf("start failure: %d %v", code, body)
	}
	if code, _ := call(t, h, "GET", "/v1/apps/nope", "", true); code != 404 {
		t.Errorf("missing app: %d", code)
	}
	if code, body := call(t, h, "GET", "/v1/apps", "", true); code != 200 || len(body["apps"].([]any)) != 1 {
		t.Errorf("list: %d %v", code, body)
	}
	if code, body := call(t, h, "POST", "/v1/apps/acme/exec", `{"command":["echo","hi"]}`, true); code != 200 || body["stdout"] != "echo hi" {
		t.Errorf("exec: %d %v", code, body)
	}
	if code, _ := call(t, h, "GET", "/v1/apps/acme/logs?tail=0", "", true); code != 400 {
		t.Errorf("bad tail: %d", code)
	}
	if code, _ := call(t, h, "DELETE", "/v1/apps/acme/volume", "", true); code != 409 {
		t.Errorf("volume before app: %d", code)
	}
	if code, _ := call(t, h, "DELETE", "/v1/apps/acme", "", true); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := call(t, h, "DELETE", "/v1/apps/acme/volume", "", true); code != 204 {
		t.Errorf("volume after app: %d", code)
	}
	if code, _ := call(t, h, "GET", "/nope", "", true); code != 404 {
		t.Errorf("unknown route: %d", code)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
