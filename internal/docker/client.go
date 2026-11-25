// Package docker is a small client for the Docker Engine API. It covers only what Apperator
// needs, so the binary has no dependencies outside the Go standard library.
package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// APIVersion is the Docker Engine API version Apperator speaks (Docker 25 and later).
const APIVersion = "v1.44"

// Client talks to one Docker daemon.
type Client struct {
	http   *http.Client
	stream *http.Client // no timeout, for event streams, pulls and exec
	base   string
}

// Error is an error response from the daemon.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("docker: %s (HTTP %d)", e.Message, e.Status) }

// IsNotFound reports whether err is a 404 from the daemon.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// IsConflict reports whether err is a 409 from the daemon.
func IsConflict(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusConflict
}

// New connects to host, for example "unix:///var/run/docker.sock" or "tcp://127.0.0.1:2375".
// An empty host uses DOCKER_HOST, then the default socket.
func New(host string) (*Client, error) {
	if host == "" {
		host = os.Getenv("DOCKER_HOST")
	}
	if host == "" {
		host = "unix:///var/run/docker.sock"
		// Docker Desktop on macOS may only have the socket in the user's home.
		if home, err := os.UserHomeDir(); err == nil {
			if _, err := os.Stat("/var/run/docker.sock"); err != nil {
				if _, err := os.Stat(home + "/.docker/run/docker.sock"); err == nil {
					host = "unix://" + home + "/.docker/run/docker.sock"
				}
			}
		}
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("docker host %q: %w", host, err)
	}
	transport := &http.Transport{MaxIdleConns: 16, IdleConnTimeout: 30 * time.Second}
	base := ""
	switch u.Scheme {
	case "unix":
		path := u.Path
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
		base = "http://docker"
	case "tcp", "http":
		base = "http://" + u.Host
	default:
		return nil, fmt.Errorf("docker host %q: only unix:// and tcp:// are supported", host)
	}
	return &Client{
		http:   &http.Client{Transport: transport, Timeout: 2 * time.Minute},
		stream: &http.Client{Transport: transport},
		base:   base + "/" + APIVersion,
	}, nil
}

func (c *Client) do(ctx context.Context, hc *http.Client, method, path string, query url.Values, body any, header http.Header) (*http.Response, error) {
	var r io.Reader
	raw, isRaw := body.(io.Reader)
	if isRaw {
		r = raw
	} else if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	if body != nil && !isRaw {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(msg, &e) == nil && e.Message != "" {
			return nil, &Error{Status: res.StatusCode, Message: e.Message}
		}
		return nil, &Error{Status: res.StatusCode, Message: strings.TrimSpace(string(msg))}
	}
	return res, nil
}

// call sends a request and decodes a JSON response into out, when out is not nil.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	res, err := c.do(ctx, c.http, method, path, query, body, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func filters(f map[string][]string) url.Values {
	b, _ := json.Marshal(f)
	return url.Values{"filters": {string(b)}}
}

// Ping checks the daemon answers.
func (c *Client) Ping(ctx context.Context) error {
	return c.call(ctx, http.MethodGet, "/_ping", nil, nil, nil)
}

// ImageID returns the ID of a local image, or a not-found error.
func (c *Client) ImageID(ctx context.Context, ref string) (string, error) {
	var img struct {
		ID string `json:"Id"`
	}
	if err := c.call(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, &img); err != nil {
		return "", err
	}
	return img.ID, nil
}

// ImagePull pulls ref. auth is an X-Registry-Auth value, or empty for public images.
func (c *Client) ImagePull(ctx context.Context, ref, auth string) error {
	h := http.Header{}
	if auth != "" {
		h.Set("X-Registry-Auth", auth)
	}
	res, err := c.do(ctx, c.stream, http.MethodPost, "/images/create", url.Values{"fromImage": {ref}}, nil, h)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	dec := json.NewDecoder(res.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("docker: pull %s: %w", ref, err)
		}
		if msg.Error != "" {
			return fmt.Errorf("docker: pull %s: %s", ref, msg.Error)
		}
	}
}

// EnsureNetwork creates a bridge network unless it exists.
func (c *Client) EnsureNetwork(ctx context.Context, name string, labels map[string]string) error {
	err := c.call(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, nil, nil)
	if !IsNotFound(err) {
		return err
	}
	err = c.call(ctx, http.MethodPost, "/networks/create", nil, map[string]any{"Name": name, "Driver": "bridge", "Labels": labels}, nil)
	if IsConflict(err) {
		return nil
	}
	return err
}

// EnsureVolume creates a named volume. Docker returns the existing one if it exists.
func (c *Client) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	return c.call(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, nil)
}

// VolumeExists reports whether a named volume exists.
func (c *Client) VolumeExists(ctx context.Context, name string) (bool, error) {
	err := c.call(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, nil, nil)
	if IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// RemoveVolume deletes a named volume and its data.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil, nil, nil)
}

// Container is the part of a container's inspect output Apperator uses.
type Container struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Image  string `json:"Image"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Restarting bool   `json:"Restarting"`
		OOMKilled  bool   `json:"OOMKilled"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// Summary is one entry of a container list.
type Summary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// Inspect returns a container by name or ID.
func (c *Client) Inspect(ctx context.Context, name string) (*Container, error) {
	var ctr Container
	if err := c.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, nil, &ctr); err != nil {
		return nil, err
	}
	return &ctr, nil
}

// List returns all containers, running or not, that have the label (key=value or key).
func (c *Client) List(ctx context.Context, label string) ([]Summary, error) {
	q := filters(map[string][]string{"label": {label}})
	q.Set("all", "1")
	var out []Summary
	err := c.call(ctx, http.MethodGet, "/containers/json", q, nil, &out)
	return out, err
}

// Create creates a container from a Docker create body and returns its ID.
func (c *Client) Create(ctx context.Context, name string, body any) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := c.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, body, &out)
	return out.ID, err
}

// Start starts a container. Starting a running container is not an error.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

// Stop stops a container, waiting up to timeout seconds before killing it.
func (c *Client) Stop(ctx context.Context, id string, timeout int) error {
	return c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", url.Values{"t": {fmt.Sprint(timeout)}}, nil, nil)
}

// Remove removes a container, stopping it first. Named volumes are kept.
func (c *Client) Remove(ctx context.Context, id string) error {
	err := c.call(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), url.Values{"force": {"1"}}, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ReadFile reads one file from a container, running or stopped, up to max bytes.
func (c *Client) ReadFile(ctx context.Context, id, path string, max int64) ([]byte, error) {
	res, err := c.do(ctx, c.http, http.MethodGet, "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {path}}, nil, nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	tr := tar.NewReader(res.Body)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, &Error{Status: http.StatusNotFound, Message: path + " is not a file"}
		}
		if err != nil {
			return nil, fmt.Errorf("docker: read %s: %w", path, err)
		}
		if h.Typeflag == tar.TypeReg {
			if h.Size > max {
				return nil, fmt.Errorf("docker: %s is larger than %d bytes", path, max)
			}
			return io.ReadAll(io.LimitReader(tr, max))
		}
	}
}

// PutFile writes one file into a container, running or not, creating its folders. The file and
// its folders belong to root.
func (c *Client) PutFile(ctx context.Context, id, path string, data []byte, mode int64) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	name := strings.TrimPrefix(path, "/")
	parts := strings.Split(name, "/")
	for i := 1; i < len(parts); i++ {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: strings.Join(parts[:i], "/") + "/", Mode: 0o755}); err != nil {
			return err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(data))}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	res, err := c.do(ctx, c.http, http.MethodPut, "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {"/"}}, &buf,
		http.Header{"Content-Type": {"application/x-tar"}})
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, res.Body)
	return res.Body.Close()
}

// ExecResult is the outcome of a command run in a container.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// Exec runs cmd in a running container and waits for it. Output is capped at max bytes per stream.
func (c *Client) Exec(ctx context.Context, id string, cmd, env []string, user string, max int) (*ExecResult, error) {
	var created struct {
		ID string `json:"Id"`
	}
	body := map[string]any{"Cmd": cmd, "Env": env, "User": user, "AttachStdout": true, "AttachStderr": true}
	if err := c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", nil, body, &created); err != nil {
		return nil, err
	}
	res, err := c.do(ctx, c.stream, http.MethodPost, "/exec/"+created.ID+"/start", nil, map[string]any{"Detach": false, "Tty": false}, nil)
	if err != nil {
		return nil, err
	}
	stdout, stderr := &capped{max: max}, &capped{max: max}
	err = Demux(res.Body, stdout, stderr)
	res.Body.Close()
	if err != nil {
		return nil, err
	}
	var info struct {
		ExitCode int  `json:"ExitCode"`
		Running  bool `json:"Running"`
	}
	if err := c.call(ctx, http.MethodGet, "/exec/"+created.ID+"/json", nil, nil, &info); err != nil {
		return nil, err
	}
	return &ExecResult{ExitCode: info.ExitCode, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// Logs returns the last lines of a container's output, stdout and stderr together.
func (c *Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {fmt.Sprint(tail)}, "timestamps": {"1"}}
	res, err := c.do(ctx, c.http, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", q, nil, nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	out := &capped{max: 4 << 20}
	if err := Demux(res.Body, out, out); err != nil {
		return "", err
	}
	return out.String(), nil
}

// Event is one message from the daemon's event stream.
type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	TimeNano int64 `json:"timeNano"`
}

// Events streams container events for containers with the label until ctx ends or the stream breaks.
func (c *Client) Events(ctx context.Context, label string, fn func(Event)) error {
	q := filters(map[string][]string{"type": {"container"}, "label": {label}})
	res, err := c.do(ctx, c.stream, http.MethodGet, "/events", q, nil, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	dec := json.NewDecoder(bufio.NewReader(res.Body))
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("docker: event stream: %w", err)
		}
		fn(e)
	}
}

// Demux splits Docker's multiplexed stdout/stderr stream (used when a container has no TTY).
func Demux(r io.Reader, stdout, stderr io.Writer) error {
	var h [8]byte
	for {
		if _, err := io.ReadFull(r, h[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("docker: stream: %w", err)
		}
		size := int64(h[4])<<24 | int64(h[5])<<16 | int64(h[6])<<8 | int64(h[7])
		w := stdout
		if h[0] == 2 {
			w = stderr
		}
		if _, err := io.CopyN(w, r, size); err != nil {
			return fmt.Errorf("docker: stream: %w", err)
		}
	}
}

// capped is a buffer that keeps the first max bytes and drops the rest.
type capped struct {
	bytes.Buffer
	max int
}

func (b *capped) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}
