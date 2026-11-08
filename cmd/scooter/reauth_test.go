package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEmacs stands in for `emacsclient --eval (scooter-auth-headers-json
// server {retry})`: {retry} t forgets the cached cookie, and with none cached
// it "prompts", answering with the answer file or, without one, declining.
// While the busy file exists another shell's prompt is open, and it answers
// null.
type fakeEmacs struct{ dir string }

func newFakeEmacs(t *testing.T, cookie string) fakeEmacs {
	e := fakeEmacs{t.TempDir()}
	e.write(t, "cookie", cookie)
	return e
}

func (e fakeEmacs) write(t *testing.T, name, s string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, name), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e fakeEmacs) source() headerSource {
	return headerSource{command: fmt.Sprintf(`d=%s
# Another shell's prompt is open: Emacs answers null, retry or not.
if [ -f $d/busy ]; then echo asked-busy >> $d/calls; echo null; exit 0; fi
if [ "{retry}" = t ]; then : > $d/cookie; fi
if [ ! -s $d/cookie ]; then
  echo asked >> $d/prompts
  sleep 0.2
  [ -f $d/answer ] || { echo 'Quit' >&2; exit 1; }
  cp $d/answer $d/cookie
fi
printf '{"Cookie":"_oauth2_proxy=%%s"}' "$(cat $d/cookie)"`, e.dir)}
}

// prompts counts the sign-in prompts so far.
func (e fakeEmacs) prompts() int {
	b, _ := os.ReadFile(filepath.Join(e.dir, "prompts"))
	return strings.Count(string(b), "asked")
}

func TestConcurrentRefusalsShareOneSignIn(t *testing.T) {
	f, srv := newFake(t)
	f.session = "fresh"
	emacs := newFakeEmacs(t, "stale")
	emacs.write(t, "answer", "fresh")
	c := newClient(srv.URL, emacs.source())
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.call(context.Background(), "GET", "/whoami", nil, nil)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("call: %v", err)
		}
	}
	if n := emacs.prompts(); n != 1 {
		t.Errorf("signed in %d times for 5 refusals, want 1", n)
	}
}

// expiredSession opens a session whose cookie then expires, with the
// streams cut, as when oauth2-proxy's session ends mid-conversation.
func expiredSession(t *testing.T, f *fakeScooter, srv *httptest.Server, emacs fakeEmacs) *testClient {
	t.Helper()
	f.session = "first"
	tc := startBridgeWith(t, srv.URL, agentOptions{}, emacs.source())
	tc.call(t, "initialize", map[string]any{"protocolVersion": 1}, nil)
	tc.call(t, "session/new", map[string]any{"cwd": "/tmp"}, nil)
	streams := func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.subs) == 1 && len(f.listSubs) == 1 }
	waitFor(t, streams)
	f.expire("fresh")
	return tc
}

func TestStreamsReconnectAfterOneSignIn(t *testing.T) {
	f, srv := newFake(t)
	emacs := newFakeEmacs(t, "first")
	emacs.write(t, "answer", "fresh")
	tc := expiredSession(t, f, srv, emacs)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.subs) == 1 && len(f.listSubs) == 1 })
	run(f, "from the web", map[string]any{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a1", "delta": "after sign-in"})
	waitFor(t, func() bool { return strings.Contains(tc.text("agent_message_chunk"), "after sign-in") })
	if n := emacs.prompts(); n != 1 {
		t.Errorf("signed in %d times, want 1 for both streams", n)
	}
}

func TestDeclinedSignInIsNotReaskedByStreams(t *testing.T) {
	f, srv := newFake(t)
	emacs := newFakeEmacs(t, "first")
	tc := expiredSession(t, f, srv, emacs)
	// The streams' backoff would reconnect at least twice in this time.
	time.Sleep(3500 * time.Millisecond)
	if n := emacs.prompts(); n != 1 {
		t.Fatalf("asked %d times while idle after a declined sign-in, want 1", n)
	}
	// The user's next request asks again; the streams follow it at once.
	emacs.write(t, "answer", "fresh")
	tc.call(t, "session/list", map[string]any{}, nil)
	if n := emacs.prompts(); n != 2 {
		t.Errorf("prompts = %d after a request, want 2", n)
	}
	deadline := time.Now().Add(time.Second)
	for {
		f.mu.Lock()
		back := len(f.subs) == 1 && len(f.listSubs) == 1
		f.mu.Unlock()
		if back {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("streams not back within 1s of the sign-in")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPermissionAnsweredWhileSignedOut(t *testing.T) {
	f, srv := newFake(t)
	f.onMessage = interruptRun("", nil, map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"})
	emacs := newFakeEmacs(t, "first")
	f.session = "first"
	tc := startBridgeWith(t, srv.URL, agentOptions{}, emacs.source())
	tc.hold = make(chan struct{})
	startPrompt(t, tc, nil)
	waitFor(t, func() bool { return tc.request("session/request_permission") != nil })
	f.expire("fresh")
	emacs.write(t, "answer", "fresh")
	close(tc.hold)
	if r := nextResume(t, f); r.Payload.OptionID != "allow" {
		t.Errorf("resume = %+v", r)
	}
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.subs) == 1 && len(f.listSubs) == 1 })
	if n := emacs.prompts(); n != 1 {
		t.Errorf("signed in %d times, want 1", n)
	}
}

// Another shell's bridge already has Emacs's cookie prompt open, so this
// bridge's emacsclient call runs nested inside it and gets null. The request
// waits for that sign-in and goes through with its cookie, no second prompt.
func TestSignInOpenInAnotherShellIsWaitedFor(t *testing.T) {
	f, srv := newFake(t)
	emacs := newFakeEmacs(t, "stale")
	c := newClient(srv.URL, emacs.source())
	if err := c.call(context.Background(), "GET", "/whoami", nil, nil); err != nil {
		t.Fatal(err)
	}
	f.expire("fresh")
	emacs.write(t, "busy", "")
	done := make(chan error, 1)
	go func() { done <- c.call(context.Background(), "GET", "/whoami", nil, nil) }()
	time.Sleep(700 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("call returned while the other prompt was open: %v", err)
	default:
	}
	// The other shell's user pastes the cookie; its prompt closes.
	emacs.write(t, "cookie", "fresh")
	if err := os.Remove(filepath.Join(emacs.dir, "busy")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("call: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("call still waiting after the other sign-in finished")
	}
	if n := emacs.prompts(); n != 0 {
		t.Errorf("prompted %d times, want 0: the other shell's sign-in serves this one", n)
	}
}
