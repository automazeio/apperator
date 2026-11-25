package engine

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/automazeio/apperator/internal/docker"
)

// retryWakeAfter keeps a failing wake-up from being retried every tick.
const retryWakeAfter = 5 * time.Minute

// RunScheduler wakes sleeping apps when their wake file says so, and once per SafetyWake as a
// safety net, until ctx ends.
func (e *Engine) RunScheduler(ctx context.Context) {
	t := time.NewTicker(e.cfg.Tick)
	defer t.Stop()
	for {
		// Retries a routes file that couldn't be written, and picks up a file someone removed.
		e.syncRoutesLogged()
		e.checkWakes(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (e *Engine) checkWakes(ctx context.Context, now time.Time) {
	containers, err := e.docker.List(ctx, labelManaged+"=true")
	if err != nil {
		e.log.Warn("scheduler: cannot list containers", "err", err)
		return
	}
	byApp := map[string]docker.Summary{}
	running := 0
	for _, c := range containers {
		if c.Labels[labelApp] != "" {
			byApp[c.Labels[labelApp]] = c
		}
		if c.State == "running" {
			running++
		}
	}
	for _, rec := range e.store.List() {
		app := e.fromRecord(rec)
		c, ok := byApp[rec.ID]
		if app.Spec.IdleTimeout == "" || rec.Held || !ok || stateOf(c.State) != StateStopped {
			continue
		}
		reason := e.wakeReason(ctx, rec.ID, &app.Spec, now)
		if reason == "" {
			continue
		}
		if e.cfg.MaxRunning > 0 && running >= e.cfg.MaxRunning {
			e.log.Warn("scheduler: too many apps running, wake-ups wait", "running", running)
			return
		}
		running++
		go e.wake(ctx, rec.ID, reason)
	}
}

// wakeReason reads the wake file once after each stop, and says why the app should wake now, or
// returns "".
func (e *Engine) wakeReason(ctx context.Context, id string, spec *Spec, now time.Time) string {
	ctr, err := e.docker.Inspect(ctx, e.name(id))
	if err != nil {
		return ""
	}
	e.wakeMu.Lock()
	w := e.wakes[id]
	if w == nil {
		w = &wakeState{}
		e.wakes[id] = w
	}
	fresh := w.finishedAt != ctr.State.FinishedAt
	e.wakeMu.Unlock()

	if fresh {
		at := time.Time{}
		if spec.WakeFile != "" {
			at = e.readWakeFile(ctx, ctr.ID, spec.WakeFile)
		}
		e.wakeMu.Lock()
		w.finishedAt, w.at = ctr.State.FinishedAt, at
		e.wakeMu.Unlock()
	}

	e.wakeMu.Lock()
	defer e.wakeMu.Unlock()
	if now.Sub(w.lastAttempt) < retryWakeAfter {
		return ""
	}
	reason := ""
	if !w.at.IsZero() && !now.Before(w.at) {
		reason = "wake_file"
	} else if stopped, err := time.Parse(time.RFC3339Nano, ctr.State.FinishedAt); err == nil && e.cfg.SafetyWake > 0 && now.Sub(stopped) >= e.cfg.SafetyWake {
		reason = "safety"
	}
	if reason != "" {
		w.lastAttempt = now
	}
	return reason
}

// readWakeFile returns the time in the wake file, or zero when it is missing, empty or not a time.
func (e *Engine) readWakeFile(ctx context.Context, containerID, path string) time.Time {
	b, err := e.docker.ReadFile(ctx, containerID, path, 256)
	if err != nil {
		if !docker.IsNotFound(err) {
			e.log.Warn("scheduler: cannot read wake file", "container", containerID[:12], "err", err)
		}
		return time.Time{}
	}
	return ParseWakeTime(string(b))
}

// ParseWakeTime parses a wake file: an RFC 3339 time, or empty for "no wake-up planned".
func ParseWakeTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t
}

func (e *Engine) wake(ctx context.Context, id, reason string) {
	defer e.recoverPanic("wake " + id)
	unlock := e.lock(id)
	rec, err := e.store.Get(id)
	if err != nil || rec.Held {
		unlock()
		return
	}
	app := e.fromRecord(rec)
	err = e.start(ctx, id, &app.Spec)
	unlock()
	if err != nil {
		e.log.Warn("scheduler: wake-up failed", "app", id, "reason", reason, "err", err)
		e.hooks.Send("app.failed", id, map[string]any{"error": err.Error(), "wake": reason})
		return
	}
	e.log.Info("scheduler: woke app", "app", id, "reason", reason)
	e.wakeMu.Lock()
	if w := e.wakes[id]; w != nil {
		w.at = time.Time{}
	}
	e.wakeMu.Unlock()
}

// WatchEvents turns Docker's container events into webhook events until ctx ends, reconnecting
// when the stream breaks.
func (e *Engine) WatchEvents(ctx context.Context) {
	for ctx.Err() == nil {
		err := e.docker.Events(ctx, labelManaged+"=true", e.onEvent)
		if ctx.Err() != nil {
			return
		}
		e.log.Warn("docker events: reconnecting", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (e *Engine) onEvent(ev docker.Event) {
	app := ev.Actor.Attributes[labelApp]
	if app == "" {
		return
	}
	switch ev.Action {
	case "start":
		e.hooks.Send("app.started", app, nil)
	case "die":
		e.hooks.Send("app.stopped", app, map[string]any{"exit_code": exitCode(ev.Actor.Attributes["exitCode"])})
	case "oom":
		e.hooks.Send("app.oom", app, nil)
	}
}

func exitCode(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
