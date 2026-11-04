package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

type configOption struct {
	ID           string
	CurrentValue string
	Options      []struct{ Value, Name string }
}

func TestModelPickerSendsThePickOverAgui(t *testing.T) {
	f, srv := newFake(t)
	f.model = "opus"
	f.onMessage = func(f *fakeScooter, text string) {
		run(f, text,
			map[string]any{"type": "TEXT_MESSAGE_START", "messageId": "a1", "role": "assistant"},
			map[string]any{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a1", "delta": "ok"},
		)
	}
	tc := startBridge(t, srv.URL, agentOptions{})
	var ns struct {
		SessionID     string
		ConfigOptions []configOption
	}
	tc.call(t, "session/new", map[string]any{"cwd": "/tmp"}, &ns)
	if len(ns.ConfigOptions) != 1 || ns.ConfigOptions[0].ID != "model" || ns.ConfigOptions[0].CurrentValue != "opus" {
		t.Fatalf("configOptions = %+v", ns.ConfigOptions)
	}
	if o := ns.ConfigOptions[0].Options; len(o) != 2 || o[0].Name != "sonnet (default)" || o[1].Value != "opus" {
		t.Errorf("options = %+v", o)
	}

	// No pick yet: the prompt keeps the queue endpoint.
	time.Sleep(100 * time.Millisecond)
	var res struct{ StopReason string }
	tc.call(t, "session/prompt", map[string]any{"sessionId": ns.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "one"}}}, &res)
	if got := <-f.messages; got != "one" {
		t.Errorf("message = %q", got)
	}

	var set struct{ ConfigOptions []configOption }
	tc.call(t, "session/set_config_option", map[string]any{"sessionId": ns.SessionID, "configId": "model", "value": "sonnet"}, &set)
	if set.ConfigOptions[0].CurrentValue != "sonnet" {
		t.Errorf("after set: %+v", set.ConfigOptions)
	}
	tc.call(t, "session/prompt", map[string]any{"sessionId": ns.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "two"}}}, &res)
	if res.StopReason != "end_turn" {
		t.Errorf("stopReason = %q", res.StopReason)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) != 1 || f.prompts[0].model != "sonnet" || string(f.prompts[0].content) != `"two"` {
		t.Errorf("agui prompts = %+v", f.prompts)
	}
}

func TestUnknownConfigOptionIsRefused(t *testing.T) {
	_, srv := newFake(t)
	tc := startBridge(t, srv.URL, agentOptions{})
	var ns struct{ SessionID string }
	tc.call(t, "session/new", map[string]any{"cwd": "/tmp"}, &ns)
	err := tc.rpc.request(context.Background(), "session/set_config_option", map[string]any{"sessionId": ns.SessionID, "configId": "mode", "value": "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown config option") {
		t.Errorf("err = %v", err)
	}
}

func TestAguiRunErrorBeforeStartEndsThePrompt(t *testing.T) {
	f, srv := newFake(t)
	f.aguiError = "quota exceeded"
	tc := startBridge(t, srv.URL, agentOptions{})
	var ns struct{ SessionID string }
	tc.call(t, "session/new", map[string]any{"cwd": "/tmp"}, &ns)
	tc.call(t, "session/set_model", map[string]any{"sessionId": ns.SessionID, "modelId": "opus"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := tc.rpc.request(ctx, "session/prompt", map[string]any{"sessionId": ns.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "x"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Errorf("err = %v", err)
	}
}
