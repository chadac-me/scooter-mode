package main

import (
	"strings"
	"testing"
)

func TestNewLinksAreAnnounced(t *testing.T) {
	f, srv := newFake(t)
	pr := map[string]any{"source": "github", "resourceType": "pull_request", "url": "https://gh/pr/1", "title": "Fix login"}
	f.rows = []map[string]any{{"id": "c1", "links": []any{pr}}}
	tc := startBridge(t, srv.URL, agentOptions{})
	tc.call(t, "session/resume", map[string]any{"sessionId": "c1"}, nil)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.listSubs) == 1 })
	issue := map[string]any{"source": "jira", "resourceType": "issue", "url": "https://jira/X-1"}
	f.upsert(map[string]any{"id": "c1", "links": []any{pr, issue}})
	f.upsert(map[string]any{"id": "c1", "links": []any{pr, issue}})
	f.upsert(map[string]any{"id": "other", "links": []any{issue}})
	waitFor(t, func() bool {
		return strings.Contains(tc.text("agent_message_chunk"), "[jira: issue](https://jira/X-1)")
	})
	if got := tc.text("agent_message_chunk"); strings.Count(got, "Linked") != 1 || strings.Contains(got, "Fix login") {
		t.Errorf("notices = %q", got)
	}
}

func TestServiceCommandsLeaveANotice(t *testing.T) {
	got := apply(newTranslator(),
		aguiEvent{Type: "TOOL_CALL_START", ToolCallID: "t1", ToolCallName: "bash"},
		aguiEvent{Type: "TOOL_CALL_ARGS", ToolCallID: "t1", Delta: `{"command":"scooter-service start app && scooter-service stop db-ui"}`},
		aguiEvent{Type: "TOOL_CALL_END", ToolCallID: "t1"},
		aguiEvent{Type: "TOOL_CALL_RESULT", ToolCallID: "t1", Content: "ok"},
	)
	var notes []string
	for _, u := range got {
		if u["sessionUpdate"] == "agent_message_chunk" {
			notes = append(notes, u["content"].(map[string]any)["text"].(string))
		}
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "`app` started") || !strings.Contains(notes[1], "`db-ui` stopped") {
		t.Errorf("notices = %q", notes)
	}
}
