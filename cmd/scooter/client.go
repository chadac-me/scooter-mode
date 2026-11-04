package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// errAuthWall: the auth proxy refused even fresh headers.
var errAuthWall = errors.New("login required")

// headerSource reads auth headers (JSON) from a command or file.
type headerSource struct {
	command  string
	file     string
	bearer   string // a token file, reread on reload since it may rotate
	spec     string // a command printing a tunnelSpec; its headers
	specFile string // a file holding a tunnelSpec, as Tramp writes before each connect
}

// runJSONCommand runs a shell command and returns its JSON output.
func runJSONCommand(ctx context.Context, command string, retry bool) ([]byte, error) {
	// {retry} lets the command drop a refused token.
	cmd := exec.CommandContext(ctx, "sh", "-c", strings.ReplaceAll(command, "{retry}", lispBool(retry)))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	out = bytes.TrimSpace(out)
	// emacsclient prints a Lisp string: a quoted, escaped JSON document.
	if len(out) > 0 && out[0] == '"' {
		var s string
		if json.Unmarshal(out, &s) == nil {
			out = []byte(s)
		}
	}
	return out, nil
}

// loadSpec runs the spec command or reads the spec file.
func (h headerSource) loadSpec(ctx context.Context, retry bool) (tunnelSpec, error) {
	var s tunnelSpec
	var out []byte
	var err error
	if h.specFile != "" {
		out, err = os.ReadFile(h.specFile)
	} else {
		out, err = runJSONCommand(ctx, h.spec, retry)
	}
	if err != nil {
		return s, fmt.Errorf("tunnel spec: %w", err)
	}
	if err := json.Unmarshal(out, &s); err != nil {
		return s, fmt.Errorf("tunnel spec: want a JSON object: %w", err)
	}
	return s, nil
}

func (h headerSource) load(ctx context.Context, retry bool) (map[string]string, error) {
	var out []byte
	var err error
	switch {
	case h.spec != "" || h.specFile != "":
		s, err := h.loadSpec(ctx, retry)
		if s.Headers == nil {
			s.Headers = map[string]string{}
		}
		return s.Headers, err
	case h.command != "":
		out, err = runJSONCommand(ctx, h.command, retry)
	case h.bearer != "":
		tok, err := os.ReadFile(h.bearer)
		if err != nil {
			return nil, fmt.Errorf("auth token: %w", err)
		}
		return map[string]string{"Authorization": "Bearer " + strings.TrimSpace(string(tok))}, nil
	case h.file != "":
		out, err = os.ReadFile(h.file)
	default:
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auth headers: %w", err)
	}
	out = bytes.TrimSpace(out)
	m := map[string]string{}
	if len(out) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("auth headers: want a JSON object of headers: %w", err)
	}
	return m, nil
}

func lispBool(b bool) string {
	if b {
		return "t"
	}
	return "nil"
}

// client calls one Scooter server's HTTP API.
type client struct {
	base    string
	http    *http.Client
	source  headerSource
	mu      sync.Mutex
	headers map[string]string
	loaded  bool
}

func newClient(base string, source headerSource) *client {
	return &client{
		base:   strings.TrimRight(base, "/"),
		source: source,
		http: &http.Client{
			// A redirect is the login page, never an API answer.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (c *client) currentHeaders(ctx context.Context, reload bool) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded && !reload {
		return c.headers, nil
	}
	h, err := c.source.load(ctx, reload)
	if err != nil {
		return nil, err
	}
	c.headers, c.loaded = h, true
	return h, nil
}

// isAuthWall reports an auth-proxy response rather than Scooter.s.
func isAuthWall(resp *http.Response) bool {
	jsonBody := strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json")
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return true
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return true
	case resp.StatusCode == http.StatusForbidden:
		// Scooter's own 403s are JSON; the proxy's are not.
		return !jsonBody
	}
	return strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html")
}

// do sends a request, reloading headers once on an auth wall.
func (c *client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	return c.send(ctx, method, c.base+path, body, nil)
}

// doWith is do with extra request headers, such as X-Agent-Model.
func (c *client) doWith(ctx context.Context, method, path string, body any, extra map[string]string) (*http.Response, error) {
	return c.send(ctx, method, c.base+path, body, extra)
}

// doURL is do for an absolute URL, such as a sandbox origin's.
func (c *client) doURL(ctx context.Context, method, rawURL string, body any) (*http.Response, error) {
	return c.send(ctx, method, rawURL, body, nil)
}

func (c *client) send(ctx context.Context, method, rawURL string, body any, extra map[string]string) (*http.Response, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		headers, err := c.currentHeaders(ctx, attempt > 0)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(rawURL, c.base+"/") {
			c.keepRefreshedCookies(resp)
		}
		if !isAuthWall(resp) {
			return resp, nil
		}
		resp.Body.Close()
		if attempt > 0 {
			return nil, fmt.Errorf("%s: %w", c.base, errAuthWall)
		}
	}
}

// call sends JSON and decodes the reply into out, if non-nil.
func (c *client) call(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b[:min(len(b), 300)])))
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

// stream passes each SSE data payload to fn until it ends.
func (c *client) stream(ctx context.Context, path string, connected func(), fn func([]byte)) error {
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	connected()
	return sseEach(resp.Body, func(b []byte) bool { fn(b); return true })
}

// sseEach passes each SSE data payload in r to fn until fn returns false
// (nil) or r ends (io.ErrUnexpectedEOF).
func sseEach(r io.Reader, fn func([]byte) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
	var data []byte
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case len(line) == 0:
			if len(data) > 0 {
				if !fn(data) {
					return nil
				}
				data = nil
			}
		case bytes.HasPrefix(line, []byte("data:")):
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))...)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

// keepRefreshedCookies folds the server's Set-Cookie into the Cookie header,
// so a session oauth2-proxy refreshes stays valid for this process.
func (c *client) keepRefreshedCookies(resp *http.Response) {
	set := resp.Cookies()
	if len(set) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	jar := c.headers["Cookie"]
	if jar == "" {
		// Bearer or no auth: the server's cookies are not ours to send.
		return
	}
	var names []string
	values := map[string]string{}
	for _, part := range strings.Split(jar, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" {
			continue
		}
		if _, dup := values[name]; !dup {
			names = append(names, name)
		}
		values[name] = value
	}
	for _, ck := range set {
		if _, had := values[ck.Name]; !had {
			names = append(names, ck.Name)
		}
		values[ck.Name] = ck.Value
	}
	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(name + "=" + values[name])
	}
	headers := make(map[string]string, len(c.headers))
	for k, v := range c.headers {
		headers[k] = v
	}
	headers["Cookie"] = b.String()
	c.headers = headers
}
