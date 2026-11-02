package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var askData = map[string]any{"questions": []any{
	map[string]any{"question": "Which approach?", "header": "Approach", "options": []any{
		map[string]any{"label": "Refactor", "description": "Safer", "preview": "diff…"},
		map[string]any{"label": "Patch"},
	}},
	map[string]any{"question": "Which checks?", "header": "Checks", "multiSelect": true, "options": []any{
		map[string]any{"label": "lint"},
		map[string]any{"label": "test"},
	}},
}}

var awsData = map[string]any{
	"account":       "prod",
	"risk":          "high",
	"justification": "rotate the key",
	"summary":       "Read and update one secret",
	"policy_document": map[string]any{"Statement": map[string]any{
		"Effect":   "Allow",
		"Action":   []any{"secretsmanager:GetSecretValue", "secretsmanager:PutSecretValue"},
		"Resource": "arn:aws:secretsmanager:us-east-1:1:secret:db",
	}},
	"write_actions": []any{"secretsmanager:PutSecretValue"},
	"obligations":   []any{map[string]any{"rules": []any{"prod-writes"}, "parts": []any{}, "approved_by": nil}},
}

// interruptRun pauses a run on one request of kind with data.
func interruptRun(kind string, data any, options ...map[string]any) func(f *fakeScooter, text string) {
	if len(options) == 0 {
		options = []map[string]any{{"optionId": "answer", "name": "Answer", "kind": "allow_once"}}
	}
	return func(f *fakeScooter, _ string) {
		f.emit(map[string]any{"type": "RUN_STARTED", "threadId": "c1", "runId": "r1"})
		f.emit(map[string]any{"type": "RUN_FINISHED", "threadId": "c1", "runId": "r1", "outcome": map[string]any{
			"type": "interrupt",
			"interrupts": []any{map[string]any{
				"id": "t9", "toolCallId": "t9", "message": "The agent needs your choice", "reason": "confirmation",
				"metadata": map[string]any{"options": options, "kind": kind, "data": data},
			}},
		}})
	}
}

// startPrompt opens a session and sends a prompt without waiting for its turn.
func startPrompt(t *testing.T, tc *testClient, capabilities map[string]any) {
	t.Helper()
	tc.call(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": capabilities}, nil)
	var ns struct{ SessionID string }
	tc.call(t, "session/new", map[string]any{"cwd": "/tmp"}, &ns)
	go func() {
		var res struct{ StopReason string }
		_ = tc.rpc.request(context.Background(), "session/prompt", map[string]any{"sessionId": ns.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "go"}}}, &res)
	}()
}

func (tc *testClient) request(method string) json.RawMessage {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for _, r := range tc.requests {
		if r.method == method {
			return r.params
		}
	}
	return nil
}

func nextResume(t *testing.T, f *fakeScooter) resumeEntry {
	t.Helper()
	select {
	case r := <-f.resumes:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no resume")
	}
	return resumeEntry{}
}

var withForms = map[string]any{"elicitation": map[string]any{"form": nil}}

func TestQuestionsAnsweredThroughForm(t *testing.T) {
	f, srv := newFake(t)
	f.onMessage = interruptRun(kindQuestions, askData)
	tc := startBridge(t, srv.URL, agentOptions{})
	tc.form = map[string]any{"action": "accept", "content": map[string]any{
		"question_0":        "Refactor",
		"question_1":        []any{"lint"},
		"question_1_custom": "lint, fuzz",
	}}
	startPrompt(t, tc, withForms)

	r := nextResume(t, f)
	if r.InterruptID != "t9" || r.Status != "resolved" || r.Payload.OptionID != "answer" {
		t.Errorf("resume = %+v", r)
	}
	want := map[string]any{"Which approach?": "Refactor", "Which checks?": "lint, fuzz"}
	got, _ := json.Marshal(r.Payload.Result["answers"])
	if w, _ := json.Marshal(want); string(got) != string(w) {
		t.Errorf("answers = %s, want %s", got, w)
	}

	var p struct {
		Mode            string
		ToolCallID      string
		RequestedSchema struct {
			Properties json.RawMessage
			Required   []string
		}
	}
	if err := json.Unmarshal(tc.request("elicitation/create"), &p); err != nil {
		t.Fatal(err)
	}
	if p.Mode != "form" || p.ToolCallID != "t9" || strings.Join(p.RequestedSchema.Required, ",") != "question_0,question_1" {
		t.Errorf("elicitation = %+v", p)
	}
	// agent-shell renders fields in the order they arrive.
	order := []string{`"question_0"`, `"question_0_custom"`, `"question_1"`, `"question_1_custom"`}
	props, last := string(p.RequestedSchema.Properties), -1
	for _, k := range order {
		i := strings.Index(props, k+":")
		if i <= last {
			t.Fatalf("fields out of order: %s", props)
		}
		last = i
	}
	for _, s := range []string{`"_askUserQuestionCustomAnswer":{"questionId":"question_0"}`, `"preview":"diff…"`, `"anyOf"`} {
		if !strings.Contains(props, s) {
			t.Errorf("schema lacks %s: %s", s, props)
		}
	}
}

func TestDeclinedFormDismisses(t *testing.T) {
	f, srv := newFake(t)
	f.onMessage = interruptRun(kindQuestions, askData)
	tc := startBridge(t, srv.URL, agentOptions{})
	tc.form = map[string]any{"action": "decline"}
	startPrompt(t, tc, withForms)
	if r := nextResume(t, f); r.Status != "cancelled" || r.Payload.OptionID != "" {
		t.Errorf("resume = %+v", r)
	}
}

func TestQuestionsWithoutFormsPointAtWebUI(t *testing.T) {
	f, srv := newFake(t)
	f.onMessage = interruptRun(kindQuestions, askData)
	tc := startBridge(t, srv.URL, agentOptions{})
	startPrompt(t, tc, nil)
	waitFor(t, func() bool { return strings.Contains(tc.text("agent_message_chunk"), "/chat/c1") })
	got := tc.text("agent_message_chunk")
	for _, s := range []string{"Which approach?", "- Refactor: Safer", "- test"} {
		if !strings.Contains(got, s) {
			t.Errorf("note lacks %q: %q", s, got)
		}
	}
	if tc.request("session/request_permission") != nil {
		t.Error("asked questions as a permission")
	}
}

func TestAWSPermissionShowsGrant(t *testing.T) {
	f, srv := newFake(t)
	f.onMessage = interruptRun(kindAWS, awsData,
		map[string]any{"optionId": "approve", "name": "Approve", "kind": "allow_once"},
		map[string]any{"optionId": "deny", "name": "Deny", "kind": "reject_once"})
	tc := startBridge(t, srv.URL, agentOptions{})
	tc.choose = "approve"
	startPrompt(t, tc, withForms)
	if r := nextResume(t, f); r.Status != "resolved" || r.Payload.OptionID != "approve" || r.Payload.Result != nil {
		t.Errorf("resume = %+v", r)
	}
	var p struct {
		ToolCall struct {
			Title   string
			Content []struct{ Content struct{ Text string } }
		}
	}
	if err := json.Unmarshal(tc.request("session/request_permission"), &p); err != nil {
		t.Fatal(err)
	}
	if p.ToolCall.Title != "AWS access to prod (high risk)" || len(p.ToolCall.Content) != 1 {
		t.Fatalf("toolCall = %+v", p.ToolCall)
	}
	body := p.ToolCall.Content[0].Content.Text
	for _, s := range []string{"Reason: rotate the key", "Allow on `arn:aws:secretsmanager:us-east-1:1:secret:db`", "- `secretsmanager:PutSecretValue` (write)", "- prod-writes: waiting"} {
		if !strings.Contains(body, s) {
			t.Errorf("body lacks %q:\n%s", s, body)
		}
	}
}

func TestUnknownKindKeepsMessage(t *testing.T) {
	in := interrupt{Message: "Pick one"}
	in.Metadata.Kind = "commits.identity"
	if title, body := describe(in); title != "Pick one" || body != "" {
		t.Errorf("describe = %q, %q", title, body)
	}
	in.Metadata.Kind = kindAWS
	in.Metadata.Data = json.RawMessage(`{"account": 3}`)
	if title, _ := describe(in); title != "Pick one" {
		t.Errorf("malformed grant title = %q", title)
	}
}

func TestFormAnswersNeedEveryQuestion(t *testing.T) {
	b, _ := json.Marshal(askData)
	qs := questionsOf(b)
	if _, ok := formAnswers(qs, map[string]any{"question_0": "Patch"}); ok {
		t.Error("accepted a form missing a question")
	}
}

func TestRejectedFormPointsAtWebUI(t *testing.T) {
	f, srv := newFake(t)
	f.onMessage = interruptRun(kindQuestions, askData)
	tc := startBridge(t, srv.URL, agentOptions{})
	startPrompt(t, tc, withForms)
	waitFor(t, func() bool { return strings.Contains(tc.text("agent_message_chunk"), "Which checks?") })
	select {
	case r := <-f.resumes:
		t.Errorf("answered a form the client refused: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
}
