package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// blocks lists the content blocks of every update of kind, tool results unwrapped.
func (tc *testClient) blocks(kind string) []map[string]any {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	var out []map[string]any
	for _, u := range tc.updates {
		if u["sessionUpdate"] != kind {
			continue
		}
		switch c := u["content"].(type) {
		case map[string]any:
			out = append(out, c)
		case []any:
			for _, item := range c {
				out = append(out, item.(map[string]any)["content"].(map[string]any))
			}
		}
	}
	return out
}

func TestReplayInlinesImages(t *testing.T) {
	f, srv := newFake(t)
	f.assets = map[string][2]string{
		"/conversations/c7/assets/u1":  {"image/png", "user-png"},
		"/conversations/c7/assets/t1":  {"image/jpeg", "tool-jpg"},
		"/conversations/c7/assets/bad": {"application/pdf", "%PDF"},
	}
	run(f, "look",
		map[string]any{"type": "TOOL_CALL_START", "toolCallId": "t1", "toolCallName": "screenshot"},
		map[string]any{"type": "TOOL_CALL_RESULT", "toolCallId": "t1", "content": `[{"type":"text","text":"took it"},{"type":"image","url":"/conversations/c7/assets/t1","mimeType":"image/jpeg"},{"type":"image","omitted":"too big","mimeType":"image/png"}]`},
		map[string]any{"type": "TEXT_MESSAGE_START", "messageId": "a1", "role": "assistant"},
		map[string]any{"type": "MESSAGE_IMAGES", "messageId": "a1", "images": []any{map[string]any{"url": "/conversations/c7/assets/bad", "mimeType": "image/png"}}},
	)
	// The user's images follow the end of their message.
	f.emit(map[string]any{"type": "MESSAGE_IMAGES", "messageId": "u1", "images": []any{map[string]any{"url": "/conversations/c7/assets/u1", "mimeType": "image/png"}}})

	tc := startBridge(t, srv.URL, agentOptions{conversation: "c7"})
	tc.call(t, "session/new", map[string]any{"cwd": "/tmp"}, nil)
	waitFor(t, func() bool { return len(tc.blocks("user_message_chunk")) == 2 })

	user := tc.blocks("user_message_chunk")[1]
	if user["type"] != "image" || user["mimeType"] != "image/png" || user["data"] != base64.StdEncoding.EncodeToString([]byte("user-png")) {
		t.Errorf("user image = %v", user)
	}
	var tool []map[string]any
	waitFor(t, func() bool { tool = tc.blocks("tool_call_update"); return len(tool) == 3 })
	if tool[0]["text"] != "took it" || tool[1]["data"] != base64.StdEncoding.EncodeToString([]byte("tool-jpg")) {
		t.Errorf("tool result = %v", tool)
	}
	if !strings.Contains(tool[2]["text"].(string), "not shown: too big") {
		t.Errorf("omitted image = %v", tool[2])
	}
	agent := tc.blocks("agent_message_chunk")
	if len(agent) != 1 || !strings.Contains(agent[0]["text"].(string), "not an image") {
		t.Errorf("non-image asset = %v", agent)
	}
	for _, b := range append(tool, agent...) {
		if _, pending := b[assetKey]; pending {
			t.Errorf("sent an unresolved block %v", b)
		}
	}
}

func TestPromptImagesGoOverAgui(t *testing.T) {
	f, srv := newFake(t)
	f.onMessage = func(f *fakeScooter, text string) { run(f, text) }
	tc := startBridge(t, srv.URL, agentOptions{})
	var init struct {
		AgentCapabilities struct{ PromptCapabilities struct{ Image bool } }
	}
	tc.call(t, "initialize", map[string]any{"protocolVersion": 1}, &init)
	if !init.AgentCapabilities.PromptCapabilities.Image {
		t.Error("image prompts not advertised")
	}
	var ns struct{ SessionID string }
	tc.call(t, "session/new", map[string]any{"cwd": "/tmp"}, &ns)
	time.Sleep(100 * time.Millisecond)
	var res struct{ StopReason string }
	tc.call(t, "session/prompt", map[string]any{"sessionId": ns.SessionID, "prompt": []any{
		map[string]any{"type": "text", "text": "what is this"},
		map[string]any{"type": "image", "mimeType": "image/png", "data": "aGk="},
	}}, &res)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) != 1 {
		t.Fatalf("prompts = %+v", f.prompts)
	}
	var parts []map[string]any
	if err := json.Unmarshal(f.prompts[0].content, &parts); err != nil || len(parts) != 2 ||
		parts[0]["text"] != "what is this" || parts[1]["type"] != "image" || parts[1]["data"] != "aGk=" {
		t.Errorf("content = %s", f.prompts[0].content)
	}
}

func TestBadPromptImagesAreRefused(t *testing.T) {
	f, srv := newFake(t)
	tc := startBridge(t, srv.URL, agentOptions{})
	var ns struct{ SessionID string }
	tc.call(t, "session/new", map[string]any{"cwd": "/tmp"}, &ns)
	big := strings.Repeat("A", (maxImageBytes/3+10)*4)
	for _, img := range []map[string]any{
		{"type": "image", "mimeType": "image/svg+xml", "data": "aGk="},
		{"type": "image", "mimeType": "image/png", "data": big},
		{"type": "image", "mimeType": "image/png"},
	} {
		err := tc.rpc.request(context.Background(), "session/prompt", map[string]any{"sessionId": ns.SessionID, "prompt": []any{img}}, nil)
		if err == nil {
			t.Errorf("accepted %v", img["mimeType"])
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) != 0 {
		t.Errorf("posted refused images: %d", len(f.prompts))
	}
}

func TestSplitResultImagesKeepsPlainResults(t *testing.T) {
	for _, raw := range []string{"plain text", `{"type":"text","text":"no images"}`, `{"image":"not a ref"}`} {
		if _, _, ok := splitResultImages(raw); ok {
			t.Errorf("split %q", raw)
		}
	}
	text, images, ok := splitResultImages(`{"content":[{"type":"image","url":"/conversations/c/assets/a"}],"note":"kept"}`)
	if !ok || len(images) != 1 || text != `{"note":"kept"}` {
		t.Errorf("text=%q images=%v", text, images)
	}
}
