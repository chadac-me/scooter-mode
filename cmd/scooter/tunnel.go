package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// tunnelSpec says where to connect, as JSON from --spec-command or --spec-file.
type tunnelSpec struct {
	URL          string            `json:"url"`
	Conversation string            `json:"conversation"`
	Headers      map[string]string `json:"headers"`
}

type tunnelOptions struct {
	server       string
	conversation string
	source       headerSource
	// wait bounds waking the sandbox and starting its ssh service.
	wait   time.Duration
	poll   time.Duration
	stdin  io.Reader
	stdout io.Writer
	log    func(string)
}

// runTunnel pipes stdio to a conversation's ssh web service.
func runTunnel(ctx context.Context, o tunnelOptions) error {
	c := newClient(o.server, o.source)
	base, err := sshServiceURL(ctx, c, o.conversation)
	if err != nil {
		return err
	}
	ws, err := connectService(ctx, c, base, o)
	if err != nil {
		return err
	}
	defer func() { _ = ws.Close() }()
	go keepAlive(ctx, ws)
	go func() { _ = ws.copyFrom(o.stdin) }()
	return ws.copyTo(o.stdout)
}

// sshServiceURL returns the ssh service's base URL, without a trailing slash.
// With a sandbox domain the main host redirects to the conversation's own
// origin; without one the service lives under /c/<id>/ on the main host.
func sshServiceURL(ctx context.Context, c *client, conversation string) (string, error) {
	path := "/c/" + url.PathEscape(conversation) + "/ssh/"
	for attempt := 0; ; attempt++ {
		headers, err := c.currentHeaders(ctx, attempt > 0)
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		if err != nil {
			return "", err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		// A navigation, the only request the proxy redirects.
		req.Header.Set("Accept", "text/html")
		resp, err := c.http.Do(req)
		if err != nil {
			return "", err
		}
		_ = resp.Body.Close()
		wall := resp.StatusCode == http.StatusUnauthorized ||
			(resp.StatusCode == http.StatusForbidden && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json"))
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc, err := resp.Location()
			if err == nil && loc.Path == "/ssh/" {
				return loc.Scheme + "://" + loc.Host + "/ssh", nil
			}
			// Anywhere else is the auth proxy's login.
			wall = true
		}
		if !wall {
			return c.base + strings.TrimSuffix(path, "/"), nil
		}
		if attempt > 0 {
			return "", fmt.Errorf("%s: %w", c.base, errAuthWall)
		}
	}
}

// serviceStatus is the proxy's loading-page poll; polling it wakes the
// sandbox and starts the unit.
type serviceStatus struct {
	Running bool   `json:"running"`
	Phase   string `json:"phase"`
}

// transientError is a failure a sleeping sandbox explains: a gateway
// error, or no answer at all.  connectService retries these.
type transientError struct{ err error }

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

// connectService waits for the ssh service and opens its WebSocket.  While
// it doesn't answer, the sandbox is woken: resumed if the conversation
// isn't running, and its ssh service started.  It retries until o.wait.
func connectService(ctx context.Context, c *client, base string, o tunnelOptions) (*wsConn, error) {
	ctx, cancel := context.WithTimeout(ctx, o.wait)
	defer cancel()
	w := waker{c: c, conversation: o.conversation, log: o.log}
	said := ""
	var last error
	for {
		st, err := pollStatus(ctx, c, base)
		var te *transientError
		switch {
		case ctx.Err() != nil:
		case errors.As(err, &te):
			last = err
		case err != nil:
			return nil, err
		case st.Running:
			ws, err := dialService(ctx, c, base+"/")
			if err == nil || !isTransientDial(err) {
				return ws, err
			}
			last = err
		default:
			if msg := map[string]string{"sandbox": "waking the sandbox", "service": "starting ssh"}[st.Phase]; msg != "" && msg != said {
				o.log(msg + "...")
				said = msg
			}
			last = errors.New("not running yet")
		}
		if ctx.Err() == nil {
			w.wake(ctx)
		}
		select {
		case <-ctx.Done():
			if ctxErr := ctx.Err(); errors.Is(ctxErr, context.Canceled) {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("ssh service not ready after %s: %w", o.wait, last)
		case <-time.After(o.poll):
		}
	}
}

// pollStatus asks the proxy whether the ssh service runs.
func pollStatus(ctx context.Context, c *client, base string) (serviceStatus, error) {
	var st serviceStatus
	resp, err := c.doURL(ctx, http.MethodGet, base+"/__scooter_status", nil)
	if err != nil {
		if errors.Is(err, errAuthWall) {
			return st, err
		}
		return st, &transientError{err}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return st, errors.New("no such conversation, or its sandbox has no ssh service (webServices.ssh.enable)")
	case resp.StatusCode >= 500:
		return st, &transientError{fmt.Errorf("ssh service status: %s", resp.Status)}
	case resp.StatusCode != http.StatusOK:
		return st, fmt.Errorf("ssh service status: %s", resp.Status)
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return st, fmt.Errorf("ssh service status: %w", err)
	}
	return st, nil
}

// isTransientDial reports a dial failure worth retrying: no connection, or
// a gateway error while sshd comes up.
func isTransientDial(err error) bool {
	var he *handshakeError
	if errors.As(err, &he) {
		return he.resp.StatusCode >= 500
	}
	return !errors.Is(err, errAuthWall)
}

// waker resumes a conversation and starts its ssh service, each until the
// server accepts it once.
type waker struct {
	c            *client
	conversation string
	log          func(string)
	resumed      bool
	started      bool
	checkFailed  bool
}

func (w *waker) wake(ctx context.Context) {
	path := "/conversations/" + url.PathEscape(w.conversation)
	if !w.resumed {
		var conv struct {
			Status string `json:"status"`
		}
		if err := w.c.call(ctx, http.MethodGet, path, nil, &conv); err != nil {
			if !w.checkFailed {
				w.log("checking the conversation: " + err.Error())
				w.checkFailed = true
			}
		} else if conv.Status == "" || conv.Status == "running" {
			w.resumed = true
		} else {
			// Once: the proxy may be resuming it too.
			w.resumed = true
			w.log("resuming the " + conv.Status + " sandbox...")
			if err := w.c.call(ctx, http.MethodPost, path+"/resume", map[string]any{}, nil); err != nil {
				w.log("resuming: " + err.Error())
			}
		}
	}
	if w.resumed && !w.started {
		if err := w.c.call(ctx, http.MethodPost, path+"/web-services/ssh/start", map[string]any{}, nil); err == nil {
			w.started = true
		}
	}
}

// dialService opens the WebSocket, reloading headers once on an auth wall.
func dialService(ctx context.Context, c *client, rawURL string) (*wsConn, error) {
	for attempt := 0; ; attempt++ {
		headers, err := c.currentHeaders(ctx, attempt > 0)
		if err != nil {
			return nil, err
		}
		ws, err := dialWebSocket(ctx, rawURL, headers)
		var he *handshakeError
		if !errors.As(err, &he) || !isAuthWall(he.resp) || attempt > 0 {
			return ws, err
		}
	}
}

// keepAlive pings so idle proxies don't drop a quiet connection.
func keepAlive(ctx context.Context, ws *wsConn) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if ws.writeFrame(opPing, nil) != nil {
				return
			}
		}
	}
}
