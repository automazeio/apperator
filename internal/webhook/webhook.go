// Package webhook posts signed events to one URL.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Event is the JSON body of a webhook request.
type Event struct {
	ID   string         `json:"id"`
	Type string         `json:"type"`
	App  string         `json:"app"`
	At   time.Time      `json:"at"`
	Data map[string]any `json:"data,omitempty"`
}

// Sender delivers events in the background, in order, with retries. Events that can't be queued
// or delivered are logged and dropped: the webhook tells, it isn't a record.
type Sender struct {
	url, secret string
	queue       chan Event
	client      *http.Client
	log         *slog.Logger
	retries     []time.Duration
}

// New returns a sender, or nil when url is empty. A nil *Sender ignores events.
func New(url, secret string, log *slog.Logger) *Sender {
	if url == "" {
		return nil
	}
	return &Sender{
		url: url, secret: secret, log: log,
		queue:   make(chan Event, 1000),
		client:  &http.Client{Timeout: 10 * time.Second},
		retries: []time.Duration{time.Second, 5 * time.Second, 30 * time.Second},
	}
}

// Send queues an event.
func (s *Sender) Send(typ, app string, data map[string]any) {
	if s == nil {
		return
	}
	e := Event{ID: "evt_" + randomHex(10), Type: typ, App: app, At: time.Now().UTC(), Data: data}
	select {
	case s.queue <- e:
	default:
		s.log.Warn("webhook queue full, event dropped", "type", typ, "app", app)
	}
}

// Run delivers queued events until ctx ends.
func (s *Sender) Run(ctx context.Context) {
	if s == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-s.queue:
			s.deliver(ctx, e)
		}
	}
}

func (s *Sender) deliver(ctx context.Context, e Event) {
	body, _ := json.Marshal(e)
	for attempt := 0; ; attempt++ {
		err := s.post(ctx, body)
		if err == nil {
			return
		}
		if attempt >= len(s.retries) || ctx.Err() != nil {
			s.log.Warn("webhook delivery failed, event dropped", "type", e.Type, "app", e.App, "err", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.retries[attempt]):
		}
	}
}

func (s *Sender) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "apperator")
	req.Header.Set("Apperator-Signature", Sign(s.secret, time.Now(), body))
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}

// Sign returns the Apperator-Signature header: "t=<unix seconds>,v1=<hex HMAC-SHA256>" over
// "<t>.<body>".
func Sign(secret string, at time.Time, body []byte) string {
	t := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "."))
	mac.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
