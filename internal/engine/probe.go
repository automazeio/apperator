package engine

import (
	"debug/elf"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"
)

// ProbePath is where Apperator puts its own binary in an app's container, to run its health check.
const ProbePath = "/.apperator/probe"

// loadProbe reads the probe binary. Docker runs it inside app containers of every kind, including
// images with no shell or libc, so it must be a static Linux executable.
func LoadProbe(path string) ([]byte, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("apperator runs on %s, and apps need a Linux binary", runtime.GOOS)
	}
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s is not a Linux executable: %w", path, err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return nil, fmt.Errorf("%s is linked dynamically; build it with CGO_ENABLED=0", path)
		}
	}
	return os.ReadFile(path)
}

// Probe checks url from inside a container and returns the exit code for Docker's health check:
// 0 when the app answers below HTTP 400, 1 otherwise.
func Probe(url string) int {
	c := &http.Client{
		Timeout:       2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := c.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	res.Body.Close()
	if res.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "HTTP %d\n", res.StatusCode)
		return 1
	}
	return 0
}
