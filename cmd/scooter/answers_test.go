package main

import (
	"strings"
	"testing"
	"time"
)

// Scooter replies to a resume only once the resumed run starts, and drops
// the answer if the client hangs up before then.

var askOptions = []map[string]any{
	{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
	{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
}

var questionForm = map[string]any{"action": "accept", "content": map[string]any{
	"question_0": "Patch", "question_1": []any{"test"},
}}

// failed is the text of the first failed update to tool call id.
func (tc *testClient) failed(id string) string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for _, u := range tc.updates {
		if u["sessionUpdate"] == "tool_call_update" && u["toolCallId"] == id && u["status"] == "failed" {
			c, _ := u["content"].([]any)
			if len(c) == 0 {
				return "(no text)"
			}
			return c[0].(map[string]any)["content"].(map[string]any)["text"].(string)
		}
	}
	return ""
}

func (f *fakeScooter) abandonedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.abandoned
}

func TestSlowResumeKeepsFormAnswer(t *testing.T) {
	f, srv := newFake(t)
	f.resumeDelay = 300 * time.Millisecond
	f.onMessage = interruptRun(kindQuestions, askData)
	tc := startBridge(t, srv.URL, agentOptions{})
	tc.form = questionForm
	startPrompt(t, tc, withForms)
	if r := nextResume(t, f); r.Status != "resolved" || r.Payload.Result["answers"] == nil {
		t.Errorf("resume = %+v", r)
	}
	if n := f.abandonedCount(); n != 0 {
		t.Errorf("hung up on %d resumes before Scooter took them", n)
	}
}

func TestSlowResumeKeepsPermissionAnswer(t *testing.T) {
	f, srv := newFake(t)
	f.resumeDelay = 300 * time.Millisecond
	f.onMessage = interruptRun("", nil, askOptions...)
	tc := startBridge(t, srv.URL, agentOptions{})
	startPrompt(t, tc, nil)
	if r := nextResume(t, f); r.Payload.OptionID != "allow" {
		t.Errorf("resume = %+v", r)
	}
	if n := f.abandonedCount(); n != 0 {
		t.Errorf("hung up on %d resumes before Scooter took them", n)
	}
}

func TestRefusedFormAnswerIsShownAndAskedAgain(t *testing.T) {
	f, srv := newFake(t)
	f.refusals = []refusal{{runError: "interrupt t9 is not waiting"}}
	f.onMessage = interruptRun(kindQuestions, askData)
	tc := startBridge(t, srv.URL, agentOptions{})
	tc.form = questionForm
	startPrompt(t, tc, withForms)
	if r := nextResume(t, f); r.Payload.Result["answers"] == nil {
		t.Errorf("resume = %+v", r)
	}
	if n := tc.count("elicitation/create"); n != 2 {
		t.Errorf("asked %d times, want 2", n)
	}
	if got := tc.failed("t9"); !strings.Contains(got, "interrupt t9 is not waiting") {
		t.Errorf("refusal shown as %q", got)
	}
}

func TestRejectedPermissionAnswerIsShownAndAskedAgain(t *testing.T) {
	f, srv := newFake(t)
	f.refusals = []refusal{{status: 409}}
	f.onMessage = interruptRun("", nil, askOptions...)
	tc := startBridge(t, srv.URL, agentOptions{})
	startPrompt(t, tc, nil)
	if r := nextResume(t, f); r.Payload.OptionID != "allow" {
		t.Errorf("resume = %+v", r)
	}
	if n := tc.count("session/request_permission"); n != 2 {
		t.Errorf("asked %d times, want 2", n)
	}
	got := tc.failed("t9")
	for _, s := range []string{"409", "interrupt t9 is not waiting"} {
		if !strings.Contains(got, s) {
			t.Errorf("refusal shown as %q, want %q in it", got, s)
		}
	}
}
