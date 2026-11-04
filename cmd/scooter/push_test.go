package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// kinds lists the session/update kinds received, in order.
func (tc *testClient) kinds() []string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	var out []string
	for _, u := range tc.updates {
		out = append(out, u["sessionUpdate"].(string))
	}
	return out
}

func (tc *testClient) count(method string) int {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	n := 0
	for _, r := range tc.requests {
		if r.method == method {
			n++
		}
	}
	return n
}

func reply(f *fakeScooter, text string) {
	run(f, "from the web",
		map[string]any{"type": "TEXT_MESSAGE_START", "messageId": "a1", "role": "assistant"},
		map[string]any{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a1", "delta": text},
		map[string]any{"type": "TEXT_MESSAGE_END", "messageId": "a1"},
	)
}

func TestWebRunArrivesAsAPush(t *testing.T) {
	f, srv := newFake(t)
	tc := startBridge(t, srv.URL, agentOptions{})
	tc.call(t, "session/resume", map[string]any{"sessionId": "c1"}, nil)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.subs) == 1 })
	reply(f, "web answer")
	waitFor(t, func() bool { k := tc.kinds(); return len(k) > 0 && k[len(k)-1] == "session_push_end" })
	if n := tc.count("session/push"); n != 1 {
		t.Errorf("%d pushes", n)
	}
	// The web user's prompt renders inside the push, not out of turn.
	if got := tc.text("user_message_chunk"); got != "from the web" {
		t.Errorf("user echo = %q", got)
	}
	if got := tc.text("agent_message_chunk"); got != "web answer" {
		t.Errorf("agent text = %q", got)
	}
}

func TestAttachDuringARunPushesAfterReplay(t *testing.T) {
	f, srv := newFake(t)
	f.emit(map[string]any{"type": "RUN_STARTED", "threadId": "c1", "runId": "r1"})
	tc := startBridge(t, srv.URL, agentOptions{})
	tc.call(t, "session/load", map[string]any{"sessionId": "c1"}, nil)
	waitFor(t, func() bool { return tc.count("session/push") == 1 })
	f.emit(map[string]any{"type": "RUN_FINISHED", "threadId": "c1", "runId": "r1"})
	waitFor(t, func() bool { k := tc.kinds(); return len(k) > 0 && k[len(k)-1] == "session_push_end" })
}

func TestOutOfBandInterruptIsNoPush(t *testing.T) {
	f, srv := newFake(t)
	tc := startBridge(t, srv.URL, agentOptions{})
	tc.choose = "allow"
	tc.call(t, "session/resume", map[string]any{"sessionId": "c1"}, nil)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.subs) == 1 })
	f.emit(map[string]any{"type": "RUN_FINISHED", "threadId": "c1", "runId": "ext-g1", "outcome": map[string]any{
		"type": "interrupt",
		"interrupts": []any{map[string]any{"id": "g1", "message": "Grant?", "metadata": map[string]any{"options": []any{
			map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
		}}}},
	}})
	<-f.resumes
	if n := tc.count("session/push"); n != 0 {
		t.Errorf("%d pushes for an out-of-band request", n)
	}
}

func TestSteering(t *testing.T) {
	f, srv := newFake(t)
	f.onMessage = func(*fakeScooter, string) {}
	tc := startBridge(t, srv.URL, agentOptions{})
	var init struct {
		Meta struct{ Steering struct{ Supported bool } } `json:"_meta"`
	}
	tc.call(t, "initialize", map[string]any{"protocolVersion": 1}, &init)
	if !init.Meta.Steering.Supported {
		t.Error("steering not advertised")
	}
	tc.call(t, "session/resume", map[string]any{"sessionId": "c1"}, nil)
	steer := func(prompt ...any) string {
		var res struct{ Outcome string }
		tc.call(t, "_session/steering", map[string]any{"sessionId": "c1", "prompt": prompt}, &res)
		return res.Outcome
	}
	if got := steer(map[string]any{"type": "text", "text": "idle"}); got != "promptRequired" {
		t.Errorf("idle steer = %q", got)
	}
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.subs) == 1 })
	f.emit(map[string]any{"type": "RUN_STARTED", "threadId": "c1", "runId": "r1"})
	waitFor(t, func() bool { return tc.count("session/push") == 1 })
	if got := steer(map[string]any{"type": "text", "text": "also this"}); got != "injected" {
		t.Errorf("steer = %q", got)
	}
	if got := <-f.messages; got != "also this" {
		t.Errorf("steered message = %q", got)
	}
	if got := steer(map[string]any{"type": "image", "mimeType": "image/png", "data": "aGk="}); got != "injected" {
		t.Errorf("image steer = %q", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var parts []map[string]any
	if len(f.prompts) != 1 || f.prompts[0].priority != priorityInterrupt || json.Unmarshal(f.prompts[0].content, &parts) != nil || parts[0]["type"] != "image" {
		t.Errorf("image steer posted %+v", f.prompts)
	}
}

func TestPromptDuringAPushEndsThePush(t *testing.T) {
	f, srv := newFake(t)
	f.onMessage = func(f *fakeScooter, _ string) {
		f.emit(map[string]any{"type": "RUN_FINISHED", "threadId": "c1", "runId": "r1"})
		reply(f, "mine")
	}
	tc := startBridge(t, srv.URL, agentOptions{})
	tc.call(t, "session/resume", map[string]any{"sessionId": "c1"}, nil)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.subs) == 1 })
	f.emit(map[string]any{"type": "RUN_STARTED", "threadId": "c1", "runId": "r1"})
	waitFor(t, func() bool { return tc.count("session/push") == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var res struct{ StopReason string }
	if err := tc.rpc.request(ctx, "session/prompt", map[string]any{"sessionId": "c1", "prompt": []any{map[string]any{"type": "text", "text": "x"}}}, &res); err != nil {
		t.Fatal(err)
	}
	k := tc.kinds()
	if len(k) == 0 || k[0] != "session_push_end" {
		t.Errorf("updates = %v, want the push closed first", k)
	}
	if n := tc.count("session/push"); n != 1 {
		t.Errorf("%d pushes; the prompt's own run should not push", n)
	}
}
