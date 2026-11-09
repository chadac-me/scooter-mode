package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProxy plays Scooter's web-service proxy in front of an echo "sshd".
type fakeProxy struct {
	mu       sync.Mutex
	polls    int
	notReady int    // status polls answered "not running" first
	origin   string // sandbox origin to redirect to; "" serves on the main host
	login    bool   // redirect everything to the login page
	token    string // the Authorization value the proxy accepts
	noSSH    bool

	// A sleeping sandbox: the conversation's status and whether its ssh
	// service runs, changed by the resume and start endpoints.
	sleeping   bool
	status     string // the conversation's status, e.g. "suspended"
	sshRunning bool
	asleepCode int // the status poll's answer while suspended (0: JSON, not running)
	refuse     int // upgrades refused with 502 first
	resumes    int
	starts     int
}

func (p *fakeProxy) counts() (resumes, starts int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resumes, p.starts
}

// api plays the conversation endpoints the tunnel uses to wake a sandbox.
func (p *fakeProxy) api(w http.ResponseWriter, r *http.Request) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/conversations/conv-1":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"conv-1","status":"`+p.status+`"}`)
	case r.Method == http.MethodPost && r.URL.Path == "/conversations/conv-1/resume":
		p.resumes++
		p.status = "running"
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	case r.Method == http.MethodPost && r.URL.Path == "/conversations/conv-1/web-services/ssh/start":
		p.starts++
		if p.status == "running" {
			p.sshRunning = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	default:
		return false
	}
	return true
}

// sleepingStatus answers the status poll for a sleeping sandbox.
func (p *fakeProxy) sleepingStatus(w http.ResponseWriter) {
	p.mu.Lock()
	asleep, running, code := p.status != "running", p.sshRunning, p.asleepCode
	p.mu.Unlock()
	if asleep && code != 0 {
		w.WriteHeader(code)
		return
	}
	phase := "service"
	if asleep {
		phase = "sandbox"
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"running":%t,"phase":%q}`, running, phase)
}

// refuseUpgrade reports whether to answer this upgrade 502, as the proxy
// does while sshd isn't listening.
func (p *fakeProxy) refuseUpgrade() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refuse > 0 {
		p.refuse--
		return true
	}
	return p.sleeping && !p.sshRunning
}

func (p *fakeProxy) handler(prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p.login {
			http.Redirect(w, r, "/oauth2/start?rd=x", http.StatusFound)
			return
		}
		if r.Header.Get("Authorization") != p.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if p.api(w, r) {
			return
		}
		switch {
		case p.origin != "" && r.URL.Path == "/c/conv-1/ssh/":
			http.Redirect(w, r, p.origin+"/ssh/", http.StatusFound)
		case p.noSSH && strings.HasSuffix(r.URL.Path, "/__scooter_status"):
			http.NotFound(w, r)
		case p.sleeping && r.URL.Path == prefix+"/__scooter_status":
			p.sleepingStatus(w)
		case r.URL.Path == prefix+"/__scooter_status":
			p.mu.Lock()
			p.polls++
			running := p.polls > p.notReady
			p.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if running {
				_, _ = io.WriteString(w, `{"running":true,"phase":"service"}`)
			} else {
				_, _ = io.WriteString(w, `{"running":false,"phase":"sandbox"}`)
			}
		case r.URL.Path == prefix+"/" && r.Header.Get("Upgrade") == "websocket":
			if p.refuseUpgrade() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			echoWebSocket(w, r)
		case r.URL.Path == prefix+"/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<p>loading</p>")
		default:
			http.NotFound(w, r)
		}
	}
}

// echoWebSocket accepts an upgrade and echoes binary frames until close.
func echoWebSocket(w http.ResponseWriter, r *http.Request) {
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	conn, brw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
	_ = brw.Flush()
	ws := &wsConn{conn: conn, br: brw.Reader}
	for {
		op, payload, err := ws.readFrame()
		if err != nil || op == opClose {
			// Servers send unmasked frames.
			_, _ = conn.Write([]byte{0x80 | opClose, 0})
			return
		}
		frame := append([]byte{0x80 | opBinary, byte(len(payload))}, payload...)
		_, _ = conn.Write(frame)
	}
}

func startTunnel(t *testing.T, server string, src headerSource) (io.WriteCloser, *bufio.Reader, <-chan error, *[]string) {
	t.Helper()
	return startTunnelWait(t, server, src, 5*time.Second)
}

func startTunnelWait(t *testing.T, server string, src headerSource, wait time.Duration) (io.WriteCloser, *bufio.Reader, <-chan error, *[]string) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var logs []string
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	go func() {
		err := runTunnel(ctx, tunnelOptions{
			server: server, conversation: "conv-1", source: src,
			wait: wait, poll: 10 * time.Millisecond,
			stdin: inR, stdout: outW,
			log: func(s string) { logs = append(logs, s) },
		})
		_ = outW.Close()
		// Unblocks a test still writing to a tunnel that gave up.
		_ = inR.CloseWithError(err)
		done <- err
	}()
	return inW, bufio.NewReader(outR), done, &logs
}

func bearer(tok string) headerSource {
	return headerSource{command: `echo '{"Authorization":"` + tok + `"}'`}
}

func echoes(t *testing.T, in io.WriteCloser, out *bufio.Reader, done <-chan error) {
	t.Helper()
	if _, err := io.WriteString(in, "SSH-2.0-test\n"); err != nil {
		t.Fatal(err)
	}
	line, err := out.ReadString('\n')
	if err != nil || line != "SSH-2.0-test\n" {
		t.Fatalf("echo = %q, %v", line, err)
	}
	_ = in.Close()
	if err := <-done; err != nil {
		t.Fatalf("tunnel: %v", err)
	}
}

func TestTunnelOnMainHost(t *testing.T) {
	p := &fakeProxy{token: "t", notReady: 2}
	srv := httptest.NewServer(p.handler("/c/conv-1/ssh"))
	defer srv.Close()
	in, out, done, logs := startTunnel(t, srv.URL, bearer("t"))
	echoes(t, in, out, done)
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "waking") {
		t.Errorf("logs = %q, want one waking line", *logs)
	}
}

func TestTunnelFollowsSandboxOrigin(t *testing.T) {
	p := &fakeProxy{token: "t"}
	sandbox := httptest.NewServer(p.handler("/ssh"))
	defer sandbox.Close()
	p.origin = sandbox.URL
	main := httptest.NewServer(p.handler("/nowhere"))
	defer main.Close()
	in, out, done, _ := startTunnel(t, main.URL, bearer("t"))
	echoes(t, in, out, done)
}

func TestTunnelReloadsHeadersOnAuthWall(t *testing.T) {
	p := &fakeProxy{token: "fresh"}
	srv := httptest.NewServer(p.handler("/c/conv-1/ssh"))
	defer srv.Close()
	src := headerSource{command: `if [ {retry} = t ]; then echo '{"Authorization":"fresh"}'; else echo '{"Authorization":"stale"}'; fi`}
	in, out, done, _ := startTunnel(t, srv.URL, src)
	echoes(t, in, out, done)
}

func TestTunnelLoginRedirectIsAuthWall(t *testing.T) {
	p := &fakeProxy{token: "t", login: true}
	srv := httptest.NewServer(p.handler("/c/conv-1/ssh"))
	defer srv.Close()
	_, _, done, _ := startTunnel(t, srv.URL, bearer("t"))
	if err := <-done; !errors.Is(err, errAuthWall) {
		t.Fatalf("err = %v, want errAuthWall", err)
	}
}

func TestTunnelWithoutSSHService(t *testing.T) {
	p := &fakeProxy{token: "t", noSSH: true}
	srv := httptest.NewServer(p.handler("/c/conv-1/ssh"))
	defer srv.Close()
	_, _, done, _ := startTunnel(t, srv.URL, bearer("t"))
	if err := <-done; err == nil || !strings.Contains(err.Error(), "webServices.ssh") {
		t.Fatalf("err = %v, want a hint about webServices.ssh", err)
	}
}

func TestTunnelResumesSuspendedSandbox(t *testing.T) {
	// A suspended sandbox's proxy answers 503 rather than a status.
	p := &fakeProxy{token: "t", sleeping: true, status: "suspended", asleepCode: http.StatusServiceUnavailable}
	srv := httptest.NewServer(p.handler("/c/conv-1/ssh"))
	defer srv.Close()
	in, out, done, logs := startTunnel(t, srv.URL, bearer("t"))
	echoes(t, in, out, done)
	if resumes, starts := p.counts(); resumes != 1 || starts != 1 {
		t.Errorf("resumes = %d, starts = %d; want 1 and 1", resumes, starts)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "resuming") {
		t.Errorf("logs = %q, want a resuming line", *logs)
	}
}

func TestTunnelStartsSSHInRunningSandbox(t *testing.T) {
	p := &fakeProxy{token: "t", sleeping: true, status: "running"}
	srv := httptest.NewServer(p.handler("/c/conv-1/ssh"))
	defer srv.Close()
	in, out, done, _ := startTunnel(t, srv.URL, bearer("t"))
	echoes(t, in, out, done)
	if resumes, starts := p.counts(); resumes != 0 || starts != 1 {
		t.Errorf("resumes = %d, starts = %d; want 0 and 1", resumes, starts)
	}
}

func TestTunnelRetriesRefusedUpgrade(t *testing.T) {
	// The status says running before sshd listens.
	p := &fakeProxy{token: "t", status: "running", refuse: 2}
	srv := httptest.NewServer(p.handler("/c/conv-1/ssh"))
	defer srv.Close()
	in, out, done, _ := startTunnel(t, srv.URL, bearer("t"))
	echoes(t, in, out, done)
	if _, starts := p.counts(); starts != 1 {
		t.Errorf("starts = %d, want 1", starts)
	}
}

func TestTunnelGivesUpAfterWait(t *testing.T) {
	p := &fakeProxy{token: "t", status: "running", refuse: 1 << 30}
	srv := httptest.NewServer(p.handler("/c/conv-1/ssh"))
	defer srv.Close()
	start := time.Now()
	_, _, done, _ := startTunnelWait(t, srv.URL, bearer("t"), 300*time.Millisecond)
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want the refused upgrade", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("gave up after %s, want about the 300ms wait", d)
	}
}

func TestSpecCommandSuppliesHeaders(t *testing.T) {
	src := headerSource{spec: `echo '{"url":"https://s","conversation":"c","headers":{"Cookie":"a=b"}}'`}
	spec, err := src.loadSpec(context.Background(), false)
	if err != nil || spec.URL != "https://s" || spec.Conversation != "c" {
		t.Fatalf("spec = %+v, %v", spec, err)
	}
	h, err := src.load(context.Background(), false)
	if err != nil || h["Cookie"] != "a=b" {
		t.Fatalf("headers = %v, %v", h, err)
	}
}

func TestSpecFileSuppliesHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(path, []byte(`{"url":"https://s","conversation":"c","headers":{"Cookie":"a=b"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	src := headerSource{specFile: path}
	spec, err := src.loadSpec(context.Background(), false)
	if err != nil || spec.URL != "https://s" || spec.Conversation != "c" {
		t.Fatalf("spec = %+v, %v", spec, err)
	}
	h, err := src.load(context.Background(), true)
	if err != nil || h["Cookie"] != "a=b" {
		t.Fatalf("headers = %v, %v", h, err)
	}
}
