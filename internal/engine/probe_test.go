package engine

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
		case "/moved":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		default:
			http.Error(w, "no", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	for path, want := range map[string]int{"/health": 0, "/moved": 0, "/down": 1} {
		if got := Probe(srv.URL + path); got != want {
			t.Errorf("%s: got %d, want %d", path, got, want)
		}
	}
	if Probe("http://127.0.0.1:1/health") != 1 {
		t.Error("a closed port must fail")
	}
}

func TestHealthcheck(t *testing.T) {
	cfg := testConfig()
	s := validSpec()
	if _, ok := healthcheck(cfg, &s)["Test"]; ok {
		t.Error("without a probe binary, the image's own check must stay")
	}
	cfg.ProbeBinary = "/apperator"
	hc := healthcheck(cfg, &s)
	test, _ := hc["Test"].([]string)
	if len(test) != 4 || test[1] != ProbePath || test[3] != "http://127.0.0.1:3000/health" {
		t.Errorf("test: %v", hc["Test"])
	}
	s.HealthPath = ""
	if _, ok := healthcheck(cfg, &s)["Test"]; ok {
		t.Error("without health_path, the image's own check must stay")
	}
}

func TestWriteAtomic(t *testing.T) {
	path := t.TempDir() + "/routes/apperator.yml"
	for _, body := range []string{"one", "two"} {
		if err := writeAtomic(path, []byte(body)); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != body {
			t.Fatalf("got %q, %v", got, err)
		}
	}
	left, _ := filepath.Glob(filepath.Dir(path) + "/.apperator-*")
	if len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}
