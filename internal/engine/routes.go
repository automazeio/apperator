package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// syncRoutes writes Traefik's routes to cfg.RoutesFile when they changed, for Traefik's file
// provider. The file outlives Apperator, so Traefik keeps routing, and Sablier keeps waking apps,
// while Apperator is down or restarting. It writes a temporary file and renames it, so Traefik
// never reads half a file.
func (e *Engine) syncRoutes() error {
	if e.cfg.Router != "traefik" {
		return nil
	}
	e.routesMu.Lock()
	defer e.routesMu.Unlock()

	ids, specs := e.specs()
	b, err := json.MarshalIndent(TraefikConfig(e.cfg, ids, specs), "", "  ")
	if err != nil {
		return err
	}
	// JSON is valid YAML, and Traefik only reads files ending in .yml or .yaml.
	b = append([]byte("# Written by Apperator. Changes here are overwritten.\n"), append(b, '\n')...)
	if bytes.Equal(b, e.routesLast) {
		if _, err := os.Stat(e.cfg.RoutesFile); err == nil {
			return nil
		}
	}
	if err := writeAtomic(e.cfg.RoutesFile, b); err != nil {
		return fmt.Errorf("write Traefik's routes: %w", err)
	}
	e.routesLast = b
	return nil
}

// syncRoutesLogged is syncRoutes for callers that can't fail. The scheduler retries every tick.
func (e *Engine) syncRoutesLogged() {
	if err := e.syncRoutes(); err != nil {
		e.log.Error("routes not updated", "err", err)
	}
}

func (e *Engine) specs() ([]string, []*Spec) {
	recs := e.store.List()
	ids := make([]string, 0, len(recs))
	specs := make([]*Spec, 0, len(recs))
	for _, rec := range recs {
		var s Spec
		if json.Unmarshal(rec.Spec, &s) != nil {
			continue
		}
		ids = append(ids, rec.ID)
		specs = append(specs, &s)
	}
	return ids, specs
}

func writeAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// The .tmp suffix keeps Traefik from reading the file before it is complete.
	f, err := os.CreateTemp(dir, ".apperator-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// CheckWritable reports whether Apperator can write files in dir.
func CheckWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".apperator-check-*.tmp")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}
