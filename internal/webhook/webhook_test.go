package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSendSignsEvents(t *testing.T) {
	got := make(chan *http.Request, 1)
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- r
		bodies <- b
	}))
	defer srv.Close()

	s := New(srv.URL, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.Send("app.started", "acme", map[string]any{"exit_code": 0})

	select {
	case r := <-got:
		body := <-bodies
		var e Event
		if err := json.Unmarshal(body, &e); err != nil || e.Type != "app.started" || e.App != "acme" || !strings.HasPrefix(e.ID, "evt_") {
			t.Fatalf("event: %s %v", body, err)
		}
		sig := r.Header.Get("Apperator-Signature")
		sec, _ := strconv.ParseInt(strings.TrimPrefix(strings.Split(sig, ",")[0], "t="), 10, 64)
		if want := Sign("secret", time.Unix(sec, 0), body); sig != want {
			t.Errorf("signature %q, want %q", sig, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery")
	}
}

func TestNilSenderIgnoresEvents(t *testing.T) {
	var s *Sender
	s.Send("app.started", "acme", nil)
	s.Run(context.Background())
}
