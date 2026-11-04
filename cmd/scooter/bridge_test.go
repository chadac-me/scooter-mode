package main

import (
	"context"
	"encoding/json"
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

// fakeScooter serves the slice of Scooter's API the bridge uses.
type fakeScooter struct {
	t         *testing.T
	mu        sync.Mutex
	events    []json.RawMessage
	sums      []string
	subs      []chan frame
	onMessage func(f *fakeScooter, text string)
	// selfOnly serves just the router's sandbox self-access routes.
	selfOnly bool
	resumes  chan resumeEntry
	cancels  chan struct{}
	messages chan string
	// rows is GET /conversations.
	rows []map[string]any
	// model is every conversation's own model.
	model string
	// prompts are the POST /agui prompts; aguiError fails them before the run.
	prompts   []aguiPrompt
	aguiError string
	// assets are image bytes by path, served with their type.
	assets map[string][2]string
}

func newFake(t *testing.T) (*fakeScooter, *httptest.Server) {
	f := &fakeScooter{t: t, resumes: make(chan resumeEntry, 4), cancels: make(chan struct{}, 4), messages: make(chan string, 4)}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeScooter) emit(ev map[string]any) {
	b, _ := json.Marshal(ev)
	f.mu.Lock()
	defer f.mu.Unlock()
	prev := strings.Repeat("0", 64)
	if n := len(f.sums); n > 0 {
		prev = f.sums[n-1]
	}
	sum := fmt.Sprintf("sum-%d", len(f.sums)+1)
	f.events = append(f.events, b)
	f.sums = append(f.sums, sum)
	for _, ch := range f.subs {
		ch <- frame{Kind: "event", Event: b, Checksum: sum, PrevChecksum: prev}
	}
}

func (f *fakeScooter) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	writeJSON := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if f.selfOnly && !selfRouteFor(r) {
		http.Error(w, "not available to sandboxes", http.StatusNotFound)
		return
	}
	switch {
	case r.URL.Path == "/whoami":
		writeJSON(200, map[string]any{"id": "me"})
	case len(parts) == 4 && parts[2] == "assets":
		f.mu.Lock()
		a, ok := f.assets[r.URL.Path]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", a[0])
		_, _ = w.Write([]byte(a[1]))
	case r.Method == "GET" && r.URL.Path == "/conversations":
		f.mu.Lock()
		rows := f.rows
		f.mu.Unlock()
		writeJSON(200, rows)
	case r.Method == "POST" && r.URL.Path == "/conversations":
		writeJSON(201, map[string]any{"id": "c1"})
	case r.URL.Path == "/models":
		writeJSON(200, map[string]any{"default": "sonnet", "available": []string{"sonnet", "opus"}})
	case len(parts) == 2 && parts[0] == "conversations":
		writeJSON(200, map[string]any{"id": parts[1], "model": f.model})
	case len(parts) >= 3 && parts[2] == "history":
		f.mu.Lock()
		sum := strings.Repeat("0", 64)
		if n := len(f.sums); n > 0 {
			sum = f.sums[n-1]
		}
		evs := append([]json.RawMessage{}, f.events...)
		f.mu.Unlock()
		writeJSON(200, map[string]any{"events": evs, "checksum": sum})
	case len(parts) >= 3 && parts[2] == "events.integrity":
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		ch := make(chan frame, 64)
		f.mu.Lock()
		for _, e := range f.events {
			b, _ := json.Marshal(frame{Kind: "event", Event: e})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		f.subs = append(f.subs, ch)
		f.mu.Unlock()
		w.(http.Flusher).Flush()
		for {
			select {
			case fr := <-ch:
				b, _ := json.Marshal(fr)
				fmt.Fprintf(w, "data: %s\n\n", b)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	case len(parts) >= 3 && parts[2] == "messages":
		var body struct{ Text string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode: %v", err)
		}
		writeJSON(202, map[string]any{"ok": true})
		f.messages <- body.Text
		go f.onMessage(f, body.Text)
	case r.Method == "POST" && r.URL.Path == "/agui":
		var body struct {
			ThreadID string `json:"threadId"`
			Resume   []resumeEntry
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Priority int `json:"priority"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (len(body.Resume) != 1 && len(body.Messages) != 1) {
			f.t.Errorf("agui: %v %+v", err, body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if len(body.Resume) == 1 {
			f.resumes <- body.Resume[0]
			return
		}
		p := aguiPrompt{model: r.Header.Get("X-Agent-Model"), content: body.Messages[0].Content, priority: body.Priority}
		f.mu.Lock()
		f.prompts = append(f.prompts, p)
		runErr := f.aguiError
		f.mu.Unlock()
		if runErr != "" {
			fmt.Fprintf(w, "data: {\"type\":\"RUN_ERROR\",\"message\":%q}\n\n", runErr)
			return
		}
		var text string
		if json.Unmarshal(p.content, &text) != nil {
			text = string(p.content)
		}
		if f.onMessage != nil {
			go f.onMessage(f, text)
		}
	case len(parts) >= 3 && parts[2] == "cancel":
		writeJSON(202, map[string]any{"ok": true})
		f.cancels <- struct{}{}
	default:
		http.NotFound(w, r)
	}
}

// testClient is an ACP client wired to an in-process agent.
type testClient struct {
	rpc      *rpc
	mu       sync.Mutex
	updates  []map[string]any
	requests []clientRequest
	choose   string
	form     map[string]any // the elicitation/create reply
	hold     chan struct{}  // when set, permission answers wait for it to close
}

type clientRequest struct {
	method string
	params json.RawMessage
}

// resumeEntry is one answer in a POST /agui resume.
type resumeEntry struct {
	InterruptID string `json:"interruptId"`
	Status      string `json:"status"`
	Payload     struct {
		OptionID string         `json:"optionId"`
		Result   map[string]any `json:"result"`
	} `json:"payload"`
}

func startBridge(t *testing.T, server string, opts agentOptions) *testClient {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	toAgentR, toAgentW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	t.Cleanup(func() { _ = toAgentW.Close(); _ = toClientW.Close() })

	agentRPC := newRPC(toClientW)
	a := newAgent(ctx, newClient(server, headerSource{}), agentRPC, opts)
	go func() { _ = agentRPC.serve(ctx, toAgentR, a.handle, a.notify) }()

	tc := &testClient{rpc: newRPC(toAgentW), choose: "allow"}
	go func() {
		_ = tc.rpc.serve(ctx, toClientR, func(_ context.Context, method string, params json.RawMessage) (any, func(), error) {
			tc.mu.Lock()
			tc.requests = append(tc.requests, clientRequest{method, params})
			tc.mu.Unlock()
			switch method {
			case "session/request_permission":
				if tc.hold != nil {
					<-tc.hold
				}
				return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": tc.choose}}, nil, nil
			case "session/push":
				return map[string]any{}, nil, nil
			case "elicitation/create":
				if tc.form == nil {
					return nil, nil, &rpcError{Code: codeInvalidParams, Message: "no forms here"}
				}
				return tc.form, nil, nil
			}
			return nil, nil, &rpcError{Code: codeMethodNotFound, Message: method}
		}, func(method string, params json.RawMessage) {
			if method != "session/update" {
				tc.mu.Lock()
				tc.requests = append(tc.requests, clientRequest{method, params})
				tc.mu.Unlock()
				return
			}
			var p struct{ Update map[string]any }
			if err := json.Unmarshal(params, &p); err != nil {
				t.Errorf("update: %v", err)
			}
			tc.mu.Lock()
			tc.updates = append(tc.updates, p.Update)
			tc.mu.Unlock()
		})
	}()
	return tc
}

func (tc *testClient) call(t *testing.T, method string, params, out any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tc.rpc.request(ctx, method, params, out); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

// text joins the text chunks of one update kind.
func (tc *testClient) text(kind string) string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	var b strings.Builder
	for _, u := range tc.updates {
		if u["sessionUpdate"] == kind {
			b.WriteString(u["content"].(map[string]any)["text"].(string))
		}
	}
	return b.String()
}

func (tc *testClient) find(kind string) map[string]any {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for _, u := range tc.updates {
		if u["sessionUpdate"] == kind {
			return u
		}
	}
	return nil
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out")
}

func run(f *fakeScooter, text string, middle ...map[string]any) {
	f.emit(map[string]any{"type": "TEXT_MESSAGE_START", "messageId": "u1", "role": "user"})
	f.emit(map[string]any{"type": "TEXT_MESSAGE_CONTENT", "messageId": "u1", "delta": text})
	f.emit(map[string]any{"type": "TEXT_MESSAGE_END", "messageId": "u1"})
	f.emit(map[string]any{"type": "RUN_STARTED", "threadId": "c1", "runId": "r1"})
	for _, ev := range middle {
		f.emit(ev)
	}
	f.emit(map[string]any{"type": "RUN_FINISHED", "threadId": "c1", "runId": "r1"})
}

func TestPromptStreamsTextAndTools(t *testing.T) {
	f, srv := newFake(t)
	f.onMessage = func(f *fakeScooter, text string) {
		run(f, text,
			map[string]any{"type": "TEXT_MESSAGE_START", "messageId": "a1", "role": "assistant"},
			map[string]any{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a1", "delta": "hi "},
			map[string]any{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a1", "delta": "there"},
			map[string]any{"type": "TEXT_MESSAGE_END", "messageId": "a1"},
			map[string]any{"type": "TOOL_CALL_START", "toolCallId": "t1", "toolCallName": "bash"},
			map[string]any{"type": "TOOL_CALL_ARGS", "toolCallId": "t1", "delta": `{"command":"ls"}`},
			map[string]any{"type": "TOOL_CALL_END", "toolCallId": "t1"},
			map[string]any{"type": "TOOL_CALL_RESULT", "toolCallId": "t1", "messageId": "m", "content": "README.md"},
		)
	}
	tc := startBridge(t, srv.URL, agentOptions{})
	tc.call(t, "initialize", map[string]any{"protocolVersion": 1}, nil)
	var ns struct{ SessionID string }
	tc.call(t, "session/new", map[string]any{"cwd": "/tmp", "mcpServers": []any{}}, &ns)
	if ns.SessionID != "c1" {
		t.Fatalf("sessionId = %q", ns.SessionID)
	}
	time.Sleep(100 * time.Millisecond) // let the follower connect
	var res struct{ StopReason string }
	tc.call(t, "session/prompt", map[string]any{"sessionId": "c1", "prompt": []any{map[string]any{"type": "text", "text": "hello"}}}, &res)

	if res.StopReason != "end_turn" {
		t.Errorf("stopReason = %q", res.StopReason)
	}
	waitFor(t, func() bool { return tc.text("agent_message_chunk") == "hi there" })
	if got := tc.text("user_message_chunk"); got != "" {
		t.Errorf("echoed the client's own prompt: %q", got)
	}
	call := tc.find("tool_call")
	if call == nil || call["kind"] != "execute" || call["title"] != "bash" {
		t.Errorf("tool_call = %v", call)
	}
	waitFor(t, func() bool {
		tc.mu.Lock()
		defer tc.mu.Unlock()
		for _, u := range tc.updates {
			if u["sessionUpdate"] == "tool_call_update" && u["status"] == "completed" {
				return true
			}
		}
		return false
	})
}

func TestPermissionRoundTrip(t *testing.T) {
	f, srv := newFake(t)
	f.onMessage = func(f *fakeScooter, text string) {
		f.emit(map[string]any{"type": "RUN_STARTED", "threadId": "c1", "runId": "r1"})
		f.emit(map[string]any{"type": "RUN_FINISHED", "threadId": "c1", "runId": "r1", "outcome": map[string]any{
			"type": "interrupt",
			"interrupts": []any{map[string]any{
				"id": "t9", "toolCallId": "t9", "message": "Run rm?", "reason": "confirmation",
				"metadata": map[string]any{"options": []any{
					map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
					map[string]any{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
				}},
			}},
		}})
		got := (<-f.resumes).Payload.OptionID
		f.emit(map[string]any{"type": "PERMISSION_RESOLVED", "toolCallId": "t9", "optionId": got})
		f.emit(map[string]any{"type": "RUN_FINISHED", "threadId": "c1", "runId": "r1"})
	}
	tc := startBridge(t, srv.URL, agentOptions{})
	tc.choose = "allow"
	var ns struct{ SessionID string }
	tc.call(t, "session/new", map[string]any{"cwd": "/tmp"}, &ns)
	var res struct{ StopReason string }
	tc.call(t, "session/prompt", map[string]any{"sessionId": ns.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "clean up"}}}, &res)
	if res.StopReason != "end_turn" {
		t.Errorf("stopReason = %q", res.StopReason)
	}
}

func TestCancelBeforeRunStarts(t *testing.T) {
	f, srv := newFake(t)
	f.onMessage = func(*fakeScooter, string) {}
	tc := startBridge(t, srv.URL, agentOptions{})
	var ns struct{ SessionID string }
	tc.call(t, "session/new", map[string]any{"cwd": "/tmp"}, &ns)
	go func() {
		<-f.messages
		tc.rpc.notify("session/cancel", map[string]any{"sessionId": ns.SessionID})
	}()
	var res struct{ StopReason string }
	tc.call(t, "session/prompt", map[string]any{"sessionId": ns.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "x"}}}, &res)
	if res.StopReason != "cancelled" {
		t.Errorf("stopReason = %q", res.StopReason)
	}
	select {
	case <-f.cancels:
	case <-time.After(5 * time.Second):
		t.Error("cancel never reached the server")
	}
}

func TestAttachReplaysHistory(t *testing.T) {
	f, srv := newFake(t)
	run(f, "earlier question",
		map[string]any{"type": "TEXT_MESSAGE_START", "messageId": "a0", "role": "assistant"},
		map[string]any{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a0", "delta": "earlier answer"},
		map[string]any{"type": "TEXT_MESSAGE_END", "messageId": "a0"},
	)
	tc := startBridge(t, srv.URL, agentOptions{conversation: "c7"})
	var ns struct{ SessionID string }
	tc.call(t, "session/new", map[string]any{"cwd": "/tmp"}, &ns)
	if ns.SessionID != "c7" {
		t.Fatalf("sessionId = %q", ns.SessionID)
	}
	waitFor(t, func() bool { return tc.text("agent_message_chunk") == "earlier answer" })
	if got := tc.text("user_message_chunk"); got != "earlier question" {
		t.Errorf("user replay = %q", got)
	}
	// Events after attach arrive live, once.
	f.emit(map[string]any{"type": "TEXT_MESSAGE_START", "messageId": "a1", "role": "assistant"})
	f.emit(map[string]any{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a1", "delta": " and more"})
	waitFor(t, func() bool { return tc.text("agent_message_chunk") == "earlier answer and more" })
}

func TestAuthWallReloadsHeadersOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != "good" {
			http.Redirect(w, r, "/oauth2/start", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"me"}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	cmd := fmt.Sprintf(`n=$(cat %[1]s 2>/dev/null || echo 0); echo $((n+1)) > %[1]s; if [ "$n" -ge 1 ]; then echo '{"X-Token":"good"}'; else echo '{"X-Token":"stale"}'; fi`, counter)
	c := newClient(srv.URL, headerSource{command: cmd})
	if err := c.call(context.Background(), "GET", "/whoami", nil, nil); err != nil {
		t.Fatalf("after refresh: %v", err)
	}

	file := filepath.Join(dir, "h.json")
	if err := os.WriteFile(file, []byte(`{"X-Token":"bad"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c = newClient(srv.URL, headerSource{file: file})
	if err := c.call(context.Background(), "GET", "/whoami", nil, nil); !errors.Is(err, errAuthWall) {
		t.Fatalf("err = %v, want errAuthWall", err)
	}
}

func TestRetryPlaceholder(t *testing.T) {
	src := headerSource{command: `echo '{"X-Retry":"{retry}"}'`}
	for _, retry := range []bool{false, true} {
		h, err := src.load(context.Background(), retry)
		if err != nil || h["X-Retry"] != lispBool(retry) {
			t.Fatalf("retry=%v: %v, %v", retry, h, err)
		}
	}
}

func TestHeadersFromEmacsclient(t *testing.T) {
	// emacsclient --eval prints the JSON as a quoted Lisp string.
	h, err := headerSource{command: `printf '%s' '"{\"Cookie\":\"_oauth2_proxy=abc\"}"'`}.load(context.Background(), false)
	if err != nil || h["Cookie"] != "_oauth2_proxy=abc" {
		t.Fatalf("headers = %v, %v", h, err)
	}
}

func TestScooterForbiddenIsNotAnAuthWall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"not your conversation"}`))
	}))
	defer srv.Close()
	err := newClient(srv.URL, headerSource{}).call(context.Background(), "POST", "/conversations/x/cancel", nil, nil)
	if err == nil || errors.Is(err, errAuthWall) || !strings.Contains(err.Error(), "not your conversation") {
		t.Fatalf("err = %v", err)
	}
}

// selfRouteFor mirrors conversation-router's selfRoutes.
func selfRouteFor(r *http.Request) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "conversations" {
		return false
	}
	switch r.Method + " " + parts[2] {
	case "GET events", "GET events.integrity", "GET history", "POST messages", "POST cancel":
		return true
	}
	return false
}

func TestSelfAccessStaysOnOneConversation(t *testing.T) {
	f, srv := newFake(t)
	f.selfOnly = true
	f.onMessage = func(f *fakeScooter, text string) {
		f.emit(map[string]any{"type": "RUN_STARTED", "threadId": "c7", "runId": "r1"})
		f.emit(map[string]any{"type": "RUN_FINISHED", "threadId": "c7", "runId": "r1", "outcome": map[string]any{
			"type": "interrupt",
			"interrupts": []any{map[string]any{
				"id": "t9", "toolCallId": "t9", "message": "Run rm?", "reason": "confirmation",
				"metadata": map[string]any{"options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}}},
			}},
		}})
	}
	tc := startBridge(t, srv.URL, agentOptions{conversation: "c7", self: true, approveURL: "https://s.example/?thread=c7"})
	tc.call(t, "initialize", map[string]any{"protocolVersion": 1}, nil)
	for range 2 {
		var ns struct{ SessionID string }
		tc.call(t, "session/new", map[string]any{"cwd": "/tmp"}, &ns)
		if ns.SessionID != "c7" {
			t.Fatalf("sessionId = %q", ns.SessionID)
		}
	}
	go func() {
		var res struct{ StopReason string }
		_ = tc.rpc.request(context.Background(), "session/prompt", map[string]any{"sessionId": "c7", "prompt": []any{map[string]any{"type": "text", "text": "clean up"}}}, &res)
	}()
	waitFor(t, func() bool { return strings.Contains(tc.text("agent_message_chunk"), "https://s.example/?thread=c7") })
	if got := tc.text("agent_message_chunk"); !strings.Contains(got, "Run rm?") {
		t.Errorf("approval note = %q", got)
	}
	select {
	case <-f.resumes:
		t.Error("answered a permission over self-access")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBearerFileRereadOnReload(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := headerSource{bearer: file}
	got, err := h.load(context.Background(), false)
	if err != nil || got["Authorization"] != "Bearer one" {
		t.Fatalf("load = %v, %v", got, err)
	}
	if err := os.WriteFile(file, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.load(context.Background(), true); got["Authorization"] != "Bearer two" {
		t.Errorf("reload = %v", got)
	}
}

// aguiPrompt is one prompt posted to /agui.
type aguiPrompt struct {
	model    string
	content  json.RawMessage
	priority int
}
