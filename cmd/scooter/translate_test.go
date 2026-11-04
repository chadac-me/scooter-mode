package main

import (
	"fmt"
	"strings"
	"testing"
)

func apply(tr *translator, evs ...aguiEvent) []update {
	var out []update
	for _, ev := range evs {
		u, _ := tr.apply(ev)
		out = append(out, u...)
	}
	return out
}

func TestToolTitleNamesTheTarget(t *testing.T) {
	tr := newTranslator()
	got := apply(tr,
		aguiEvent{Type: "TOOL_CALL_START", ToolCallID: "t1", ToolCallName: "bash"},
		aguiEvent{Type: "TOOL_CALL_ARGS", ToolCallID: "t1", Delta: `{"command":"ls -la\nmore"}`},
		aguiEvent{Type: "TOOL_CALL_END", ToolCallID: "t1"},
	)
	if got[1]["title"] != "bash: ls -la" {
		t.Errorf("title = %v", got[1]["title"])
	}
	got = apply(tr,
		aguiEvent{Type: "TOOL_CALL_START", ToolCallID: "t2", ToolCallName: "think"},
		aguiEvent{Type: "TOOL_CALL_ARGS", ToolCallID: "t2", Delta: `{"thought":"x"}`},
		aguiEvent{Type: "TOOL_CALL_END", ToolCallID: "t2"},
	)
	if _, set := got[1]["title"]; set {
		t.Errorf("title without a target = %v", got[1]["title"])
	}
}

func TestLongResultsAreClipped(t *testing.T) {
	got := apply(newTranslator(), aguiEvent{Type: "TOOL_CALL_RESULT", ToolCallID: "t1", Content: strings.Repeat("é", maxResultChars)})
	text := got[0]["content"].([]any)[0].(map[string]any)["content"].(map[string]any)["text"].(string)
	if len(text) > maxResultChars+40 || !strings.Contains(text, "more bytes)") || !strings.HasPrefix(text, "éé") {
		t.Errorf("clipped to %d bytes: %q", len(text), text[len(text)-30:])
	}
}

func TestRunNotes(t *testing.T) {
	got := apply(newTranslator(),
		aguiEvent{Type: "SYSTEM_MESSAGE", Source: "github", Sender: "octocat", Text: "PR merged\nby bot"},
		aguiEvent{Type: "RUN_RETRYING", Attempt: 2, Max: 5, Code: "overloaded"},
		aguiEvent{Type: "CONTEXT_USAGE", UsedTokens: 1200, Window: 200000},
		aguiEvent{Type: "CONTEXT_USAGE", UsedTokens: 1, Window: 0},
	)
	if len(got) != 3 {
		t.Fatalf("updates = %v", got)
	}
	if got[0]["sessionUpdate"] != "agent_message_chunk" || got[0]["content"].(map[string]any)["text"] != "\n\n> **github (octocat):** PR merged\n> by bot\n\n" {
		t.Errorf("system message = %v", got[0])
	}
	if got[1]["sessionUpdate"] != "agent_thought_chunk" || !strings.Contains(fmt.Sprint(got[1]["content"]), "retrying 2/5 after overloaded") {
		t.Errorf("retry = %v", got[1])
	}
	if got[2]["sessionUpdate"] != "usage_update" || got[2]["used"] != 1200 || got[2]["size"] != 200000 {
		t.Errorf("usage = %v", got[2])
	}
}

func TestReplayTurnsCapsTheReplay(t *testing.T) {
	f, srv := newFake(t)
	for i := 1; i <= 3; i++ {
		run(f, fmt.Sprintf("q%d ", i),
			map[string]any{"type": "TEXT_MESSAGE_START", "messageId": fmt.Sprintf("a%d", i), "role": "assistant"},
			map[string]any{"type": "TEXT_MESSAGE_CONTENT", "messageId": fmt.Sprintf("a%d", i), "delta": fmt.Sprintf("a%d ", i)},
			map[string]any{"type": "TEXT_MESSAGE_END", "messageId": fmt.Sprintf("a%d", i)},
		)
	}
	tc := startBridge(t, srv.URL, agentOptions{replayTurns: 2})
	tc.call(t, "session/load", map[string]any{"sessionId": "c1"}, nil)
	waitFor(t, func() bool { return tc.text("agent_message_chunk") == "a2 a3 " })
	if got := tc.text("user_message_chunk"); got != "q2 q3 " {
		t.Errorf("user replay = %q", got)
	}
}
