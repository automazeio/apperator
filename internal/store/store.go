// Package store keeps each app's desired state as a JSON file, one file per app.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Record is what is stored for one app. Spec is kept as raw JSON so the store doesn't depend on
// the engine's types.
type Record struct {
	ID        string          `json:"id"`
	Spec      json.RawMessage `json:"spec"`
	Held      bool            `json:"held"` // stopped through the API: no scheduled wake-ups
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Store is safe for concurrent use.
type Store struct {
	dir  string
	mu   sync.RWMutex
	apps map[string]Record
}

// ErrNotFound is returned for an app that doesn't exist.
var ErrNotFound = errors.New("app not found")

// Open loads every app from dir/apps, creating the folder if needed.
func Open(dir string) (*Store, error) {
	s := &Store{dir: filepath.Join(dir, "apps"), apps: map[string]Record{}}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		var r Record
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("store: %s: %w", e.Name(), err)
		}
		s.apps[r.ID] = r
	}
	return s, nil
}

// Get returns one app.
func (s *Store) Get(id string) (Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.apps[id]
	if !ok {
		return Record{}, ErrNotFound
	}
	return r, nil
}

// List returns every app, sorted by ID.
func (s *Store) List() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0, len(s.apps))
	for _, r := range s.apps {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Put saves an app, creating it if needed.
func (s *Store) Put(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if old, ok := s.apps[r.ID]; ok {
		r.CreatedAt = old.CreatedAt
	} else {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, r.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	s.apps[r.ID] = r
	return nil
}

// SetHeld changes only the held flag.
func (s *Store) SetHeld(id string, held bool) error {
	r, err := s.Get(id)
	if err != nil {
		return err
	}
	if r.Held == held {
		return nil
	}
	r.Held = held
	return s.Put(r)
}

// Delete removes an app's record.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apps[id]; !ok {
		return ErrNotFound
	}
	if err := os.Remove(filepath.Join(s.dir, id+".json")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: %w", err)
	}
	delete(s.apps, id)
	return nil
}
