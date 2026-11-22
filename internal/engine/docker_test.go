package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/automazeio/apperator/internal/config"
	"github.com/automazeio/apperator/internal/docker"
	"github.com/automazeio/apperator/internal/store"
)

// TestWithDocker runs an app through its whole life on a real Docker daemon. It runs only with
// APPERATOR_DOCKER_TESTS=1.
func TestWithDocker(t *testing.T) {
	if os.Getenv("APPERATOR_DOCKER_TESTS") != "1" {
		t.Skip("set APPERATOR_DOCKER_TESTS=1 to run against Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg := &config.Config{
		Network: "apperator-test", Prefix: "apperator-test-", Router: "none",
		StartTimeout: 30 * time.Second, StopTimeout: 5, MaxStarting: 2, BlockingLimit: 30 * time.Second,
	}
	d, err := docker.New("")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := New(cfg, d, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := e.Init(ctx); err != nil {
		t.Fatal(err)
	}
	id := "it" + time.Now().Format("150405")
	t.Cleanup(func() {
		ctx := context.Background()
		_ = d.Remove(ctx, cfg.Prefix+id)
		_ = d.RemoveVolume(ctx, volumeName(cfg, id))
	})

	spec := Spec{
		Image:    "busybox:1.37",
		Command:  []string{"sh", "-c", "mkdir -p /www && echo ok > /www/health && exec httpd -f -p 8080 -h /www"},
		Env:      map[string]string{"GREETING": "one"},
		Port:     8080,
		DataPath: "/data",
		MemoryMB: 32,
	}
	// Container IPs are only reachable from the host on Linux.
	if runtime.GOOS == "linux" {
		spec.HealthPath = "/health"
	}

	app, created, err := e.Apply(ctx, id, spec)
	if err != nil || !created || app.State != StateRunning || !app.Container.UpToDate {
		t.Fatalf("create: %+v %v %v", app, created, err)
	}
	first := app.Container.ID

	run := func(cmd string) string {
		t.Helper()
		res, err := e.Exec(ctx, id, ExecRequest{Command: []string{"sh", "-c", cmd}})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("exec %q: %+v %v", cmd, res, err)
		}
		return strings.TrimSpace(res.Stdout)
	}
	if got := run("echo $GREETING"); got != "one" {
		t.Errorf("env: %q", got)
	}
	run("echo kept > /data/file")

	// The same spec changes nothing.
	app, created, err = e.Apply(ctx, id, spec)
	if err != nil || created || app.Container.ID != first {
		t.Fatalf("unchanged apply: %+v %v %v", app, created, err)
	}

	// A new env recreates the container and keeps the data.
	spec.Env = map[string]string{"GREETING": "two"}
	app, _, err = e.Apply(ctx, id, spec)
	if err != nil || app.Container.ID == first {
		t.Fatalf("changed apply: %+v %v", app, err)
	}
	if got := run("echo $GREETING; cat /data/file"); got != "two\nkept" {
		t.Errorf("after recreate: %q", got)
	}

	// The wake file can be read from the stopped container.
	run("echo 2026-11-23T10:00:00.000Z > /data/next-work-at")
	app, err = e.Stop(ctx, id)
	if err != nil || app.State != StateStopped || !app.Held {
		t.Fatalf("stop: %+v %v", app, err)
	}
	ctr, _ := d.Inspect(ctx, cfg.Prefix+id)
	if got := e.readWakeFile(ctx, ctr.ID, "/data/next-work-at"); !got.Equal(time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("wake file: %v", got)
	}
	if got := e.readWakeFile(ctx, ctr.ID, "/data/missing"); !got.IsZero() {
		t.Errorf("missing wake file: %v", got)
	}
	if _, err := e.Exec(ctx, id, ExecRequest{Command: []string{"true"}}); err == nil {
		t.Error("exec on a held app should fail")
	}

	app, err = e.Start(ctx, id)
	if err != nil || app.State != StateRunning || app.Held {
		t.Fatalf("start: %+v %v", app, err)
	}

	// A command that exits makes the start fail, with its output.
	bad := spec
	bad.Command = []string{"sh", "-c", "echo broken config >&2; exit 3"}
	_, _, err = e.Apply(ctx, id, bad)
	if se, ok := err.(*StartError); !ok || !strings.Contains(se.Message, "code 3") || !strings.Contains(se.Logs, "broken config") {
		t.Fatalf("bad command: %v", err)
	}
	if _, _, err = e.Apply(ctx, id, spec); err != nil {
		t.Fatalf("recover: %v", err)
	}

	list, err := e.List(ctx)
	if err != nil || len(list) != 1 || list[0].State != StateRunning {
		t.Fatalf("list: %+v %v", list, err)
	}

	if err := e.DeleteVolume(ctx, id); err == nil {
		t.Error("the volume of an existing app must not be deleted")
	}
	if err := e.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Inspect(ctx, cfg.Prefix+id); !docker.IsNotFound(err) {
		t.Errorf("container still there: %v", err)
	}
	if ok, _ := d.VolumeExists(ctx, volumeName(cfg, id)); !ok {
		t.Error("deleting the app must keep the volume")
	}
	if err := e.DeleteVolume(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteVolume(ctx, id); err != ErrNotFound {
		t.Errorf("second volume delete: %v", err)
	}
}
