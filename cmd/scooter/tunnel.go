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
	if err := waitForService(ctx, c, base, o); err != nil {
		return err
	}
	ws, err := dialService(ctx, c, base+"/")
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
		headers, gen, err := c.currentHeaders(ctx, false)
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
		if err := c.refresh(ctx, gen, false); err != nil {
			return "", fmt.Errorf("%s: %w: %w", c.base, errAuthWall, err)
		}
	}
}

// serviceStatus is the proxy's loading-page poll; polling it wakes the
// sandbox and starts the unit.
type serviceStatus struct {
	Running bool   `json:"running"`
	Phase   string `json:"phase"`
}

func waitForService(ctx context.Context, c *client, base string, o tunnelOptions) error {
	ctx, cancel := context.WithTimeout(ctx, o.wait)
	defer cancel()
	said := ""
	for {
		var st serviceStatus
		resp, err := c.doURL(ctx, http.MethodGet, base+"/__scooter_status", nil)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("ssh service not ready after %s", o.wait)
			}
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return errors.New("no such conversation, or its sandbox has no ssh service (webServices.ssh.enable)")
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("ssh service status: %s", resp.Status)
		}
		if err := json.Unmarshal(body, &st); err != nil {
			return fmt.Errorf("ssh service status: %w", err)
		}
		if st.Running {
			return nil
		}
		if msg := map[string]string{"sandbox": "waking the sandbox", "service": "starting ssh"}[st.Phase]; msg != "" && msg != said {
			o.log(msg + "...")
			said = msg
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ssh service not ready after %s", o.wait)
		case <-time.After(o.poll):
		}
	}
}

// dialService opens the WebSocket, reloading headers once on an auth wall.
func dialService(ctx context.Context, c *client, rawURL string) (*wsConn, error) {
	for attempt := 0; ; attempt++ {
		headers, gen, err := c.currentHeaders(ctx, false)
		if err != nil {
			return nil, err
		}
		ws, err := dialWebSocket(ctx, rawURL, headers)
		var he *handshakeError
		if !errors.As(err, &he) || !isAuthWall(he.resp) || attempt > 0 {
			return ws, err
		}
		if err := c.refresh(ctx, gen, false); err != nil {
			return nil, fmt.Errorf("%s: %w: %w", c.base, errAuthWall, err)
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
