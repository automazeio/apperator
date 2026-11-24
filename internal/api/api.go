// Package api is Apperator's REST API.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/automazeio/apperator/internal/docker"
	"github.com/automazeio/apperator/internal/engine"
)

// Apps is what the API needs from the engine.
type Apps interface {
	Apply(ctx context.Context, id string, spec engine.Spec) (*engine.App, bool, error)
	Get(ctx context.Context, id string) (*engine.App, error)
	List(ctx context.Context) ([]*engine.App, error)
	Start(ctx context.Context, id string) (*engine.App, error)
	Stop(ctx context.Context, id string) (*engine.App, error)
	Restart(ctx context.Context, id string) (*engine.App, error)
	Exec(ctx context.Context, id string, req engine.ExecRequest) (*docker.ExecResult, error)
	Logs(ctx context.Context, id string, tail int) (string, error)
	Delete(ctx context.Context, id string) error
	DeleteVolume(ctx context.Context, id string) error
	TraefikConfig() map[string]any
}

// Handler returns the API. Every route except GET /health needs "Authorization: Bearer <token>".
// GET /traefik serves Traefik's routes, with its own token (traefikToken), and is off when that is empty.
func Handler(apps Apps, token, traefikToken, version string, log *slog.Logger) http.Handler {
	h := &handler{apps: apps, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
	})
	mux.HandleFunc("GET /v1/apps", h.list)
	mux.HandleFunc("GET /v1/apps/{id}", h.get)
	mux.HandleFunc("PUT /v1/apps/{id}", h.put)
	mux.HandleFunc("DELETE /v1/apps/{id}", h.delete)
	mux.HandleFunc("POST /v1/apps/{id}/start", h.action((Apps).Start))
	mux.HandleFunc("POST /v1/apps/{id}/stop", h.action((Apps).Stop))
	mux.HandleFunc("POST /v1/apps/{id}/restart", h.action((Apps).Restart))
	mux.HandleFunc("POST /v1/apps/{id}/exec", h.exec)
	mux.HandleFunc("GET /v1/apps/{id}/logs", h.logs)
	mux.HandleFunc("DELETE /v1/apps/{id}/volume", h.deleteVolume)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such route", nil)
	})

	want := []byte("Bearer " + token)
	wantTraefik := []byte("Bearer " + traefikToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/traefik" && r.Method == http.MethodGet && traefikToken != "" &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), wantTraefik) == 1 {
			// Read-only and limited to routes, so Traefik never holds the API token.
			writeJSON(w, http.StatusOK, apps.TraefikConfig())
			return
		}
		if r.URL.Path != "/health" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required", nil)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		mux.ServeHTTP(w, r)
	})
}

type handler struct {
	apps Apps
	log  *slog.Logger
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	apps, err := h.apps.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps})
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	app, err := h.apps.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (h *handler) put(w http.ResponseWriter, r *http.Request) {
	var spec engine.Spec
	if !decode(w, r, &spec) {
		return
	}
	app, created, err := h.apps.Apply(r.Context(), r.PathValue("id"), spec)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, app)
}

func (h *handler) delete(w http.ResponseWriter, r *http.Request) {
	if err := h.apps.Delete(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) deleteVolume(w http.ResponseWriter, r *http.Request) {
	if err := h.apps.DeleteVolume(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) action(fn func(Apps, context.Context, string) (*engine.App, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, err := fn(h.apps, r.Context(), r.PathValue("id"))
		if err != nil {
			h.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, app)
	}
}

func (h *handler) exec(w http.ResponseWriter, r *http.Request) {
	var req engine.ExecRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := h.apps.Exec(r.Context(), r.PathValue("id"), req)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *handler) logs(w http.ResponseWriter, r *http.Request) {
	tail := 100
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10000 {
			writeError(w, http.StatusBadRequest, "invalid_request", "tail must be between 1 and 10000", nil)
			return
		}
		tail = n
	}
	logs, err := h.apps.Logs(r.Context(), r.PathValue("id"), tail)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(logs))
}

func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *engine.InvalidError
	var conflict *engine.ConflictError
	var image *engine.ImageError
	var start *engine.StartError
	switch {
	case errors.Is(err, engine.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such app", nil)
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, "invalid_request", "the request is not valid", map[string]any{"problems": invalid.Problems})
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, "conflict", conflict.Message, nil)
	case errors.As(err, &image):
		writeError(w, http.StatusUnprocessableEntity, "image_unavailable", image.Error(), nil)
	case errors.As(err, &start):
		writeError(w, http.StatusBadGateway, "start_failed", start.Message, map[string]any{"logs": start.Logs})
	case errors.Is(err, context.Canceled):
		// The client went away.
	default:
		h.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), nil)
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "invalid_request", "send JSON with Content-Type: application/json", nil)
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error(), nil)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	body := map[string]any{"code": code, "message": message}
	for k, v := range details {
		body[k] = v
	}
	writeJSON(w, status, map[string]any{"error": body})
}
