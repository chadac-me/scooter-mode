package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// aguiEvent holds the AG-UI fields the bridge reads.
type aguiEvent struct {
	Type         string          `json:"type"`
	MessageID    string          `json:"messageId"`
	Role         string          `json:"role"`
	Delta        string          `json:"delta"`
	ToolCallID   string          `json:"toolCallId"`
	ToolCallName string          `json:"toolCallName"`
	Content      string          `json:"content"`
	Message      string          `json:"message"`
	Source       string          `json:"source"`
	Text         string          `json:"text"`
	Cancelled    bool            `json:"cancelled"`
	RunID        string          `json:"runId"`
	Sender       string          `json:"sender"`
	Attempt      int             `json:"attempt"`
	Max          int             `json:"max"`
	Code         string          `json:"code"`
	UsedTokens   int             `json:"usedTokens"`
	Window       int             `json:"contextWindow"`
	Outcome      *runOutcome     `json:"outcome"`
	Items        json.RawMessage `json:"items"`
	Images       []imageRef      `json:"images"`
}

type runOutcome struct {
	Type       string      `json:"type"`
	Interrupts []interrupt `json:"interrupts"`
}

type interrupt struct {
	ID         string `json:"id"`
	Message    string `json:"message"`
	ToolCallID string `json:"toolCallId"`
	Metadata   struct {
		Options []permissionOption `json:"options"`
		// Kind picks the renderer for Data, e.g. "aws.permissions".
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	} `json:"metadata"`
}

// permissionOption is shared verbatim by Scooter and ACP.
type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// update is one ACP session/update payload.
type update map[string]any

func textChunk(kind, text string) update {
	return update{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": text}}
}

// turnSignal is how an event affects the turn in flight.
type turnSignal int

const (
	sigNone turnSignal = iota
	sigRunStarted
	sigRunEnded
	sigRunError
	sigInterrupt
	sigQueue
	sigResolved
)

// translator folds AG-UI events into ACP updates, tracking per-message state.
type translator struct {
	roles     map[string]string
	toolArgs  map[string]*strings.Builder
	toolNames map[string]string
	comments  map[string]*commentFilter
	// Off during our prompt: the client already shows its text.
	echoUser bool
}

func newTranslator() *translator {
	return &translator{roles: map[string]string{}, toolArgs: map[string]*strings.Builder{}, toolNames: map[string]string{}, comments: map[string]*commentFilter{}, echoUser: true}
}

func (t *translator) apply(ev aguiEvent) ([]update, turnSignal) {
	switch ev.Type {
	case "RUN_STARTED":
		return nil, sigRunStarted
	case "RUN_FINISHED":
		if ev.Outcome != nil && ev.Outcome.Type == "interrupt" {
			return nil, sigInterrupt
		}
		return nil, sigRunEnded
	case "RUN_ERROR":
		return []update{textChunk("agent_message_chunk", "\n\n**Run failed:** "+ev.Message+"\n")}, sigRunError
	case "QUEUE_UPDATED":
		return nil, sigQueue
	case "PERMISSION_RESOLVED":
		return nil, sigResolved
	case "TEXT_MESSAGE_START":
		t.roles[ev.MessageID] = ev.Role
	case "TEXT_MESSAGE_CONTENT":
		if t.roles[ev.MessageID] == "user" {
			if !t.echoUser {
				return nil, sigNone
			}
			return []update{textChunk("user_message_chunk", ev.Delta)}, sigNone
		}
		return t.agentText("agent_message_chunk", ev.MessageID, ev.Delta), sigNone
	case "TEXT_MESSAGE_END":
		// The role outlives the message: its MESSAGE_IMAGES come after the end.
		return t.agentFlush("agent_message_chunk", ev.MessageID), sigNone
	case "REASONING_MESSAGE_CONTENT":
		return t.agentText("agent_thought_chunk", ev.MessageID, ev.Delta), sigNone
	case "REASONING_MESSAGE_END":
		return t.agentFlush("agent_thought_chunk", ev.MessageID), sigNone
	case "SYSTEM_MESSAGE":
		// A note from a hook or integration, not the user: agent-shell drops user
		// chunks during a prompt and flags them outside one.
		who := ev.Source
		if ev.Sender != "" {
			who += " (" + ev.Sender + ")"
		}
		return []update{textChunk("agent_message_chunk", "\n\n> **"+who+":** "+strings.ReplaceAll(ev.Text, "\n", "\n> ")+"\n\n")}, sigNone
	case "RUN_RETRYING":
		code := ev.Code
		if code == "" {
			code = "error"
		}
		return []update{textChunk("agent_thought_chunk", fmt.Sprintf("\n[retrying %d/%d after %s]\n", ev.Attempt, ev.Max, code))}, sigNone
	case "CONTEXT_USAGE":
		if ev.Window <= 0 {
			return nil, sigNone
		}
		return []update{{"sessionUpdate": "usage_update", "used": ev.UsedTokens, "size": ev.Window}}, sigNone
	case "TOOL_CALL_START":
		t.toolArgs[ev.ToolCallID] = &strings.Builder{}
		t.toolNames[ev.ToolCallID] = ev.ToolCallName
		return []update{{
			"sessionUpdate": "tool_call",
			"toolCallId":    ev.ToolCallID,
			"title":         ev.ToolCallName,
			"kind":          toolKind(ev.ToolCallName),
			"status":        "pending",
		}}, sigNone
	case "TOOL_CALL_ARGS":
		if b := t.toolArgs[ev.ToolCallID]; b != nil {
			b.WriteString(ev.Delta)
		}
	case "TOOL_CALL_END":
		u := update{"sessionUpdate": "tool_call_update", "toolCallId": ev.ToolCallID, "status": "in_progress"}
		if b := t.toolArgs[ev.ToolCallID]; b != nil {
			var input any
			if json.Unmarshal([]byte(b.String()), &input) == nil {
				u["rawInput"] = input
				if title := toolTitle(t.toolNames[ev.ToolCallID], input); title != "" {
					u["title"] = title
				}
			}
			delete(t.toolArgs, ev.ToolCallID)
			delete(t.toolNames, ev.ToolCallID)
		}
		return []update{u}, sigNone
	case "TOOL_CALL_RESULT":
		return []update{{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    ev.ToolCallID,
			"status":        "completed",
			"content":       resultContent(ev.Content),
		}}, sigNone
	case "MESSAGE_IMAGES":
		kind := "agent_message_chunk"
		if t.roles[ev.MessageID] == "user" {
			if !t.echoUser {
				return nil, sigNone
			}
			kind = "user_message_chunk"
		}
		var out []update
		for _, ref := range ev.Images {
			out = append(out, update{"sessionUpdate": kind, "content": imageBlock(ref)})
		}
		return out, sigNone
	}
	return nil, sigNone
}

// agentText sends agent markdown with HTML comments hidden, as the UI does.
func (t *translator) agentText(kind, id, delta string) []update {
	f := t.comments[id]
	if f == nil {
		f = &commentFilter{}
		t.comments[id] = f
	}
	return chunk(kind, f.write(delta))
}

// agentFlush sends whatever the message's filter still holds.
func (t *translator) agentFlush(kind, id string) []update {
	f := t.comments[id]
	if f == nil {
		return nil
	}
	delete(t.comments, id)
	return chunk(kind, f.flush())
}

func chunk(kind, text string) []update {
	if text == "" {
		return nil
	}
	return []update{textChunk(kind, text)}
}

// toolKind guesses ACP.s tool kind from the tool name.
func toolKind(name string) string {
	n := strings.ToLower(name)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(n, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("read", "view", "cat"):
		return "read"
	case has("edit", "write", "patch", "replace"):
		return "edit"
	case has("delete", "remove"):
		return "delete"
	case has("move", "rename"):
		return "move"
	case has("grep", "glob", "search", "find"):
		return "search"
	case has("bash", "shell", "exec", "run", "command"):
		return "execute"
	case has("fetch", "http", "web"):
		return "fetch"
	case has("think"):
		return "think"
	}
	return "other"
}

// resultContent is a tool result as ACP content, its images split out.
func resultContent(raw string) []any {
	wrap := func(block map[string]any) any { return map[string]any{"type": "content", "content": block} }
	text, images, ok := splitResultImages(raw)
	if !ok {
		return []any{wrap(map[string]any{"type": "text", "text": clip(raw)})}
	}
	var out []any
	if text != "" {
		out = append(out, wrap(map[string]any{"type": "text", "text": clip(text)}))
	}
	for _, img := range images {
		out = append(out, wrap(img))
	}
	return out
}

// maxResultChars caps a tool result's text; the web UI has the rest.
const maxResultChars = 20000

func clip(s string) string {
	if len(s) <= maxResultChars {
		return s
	}
	cut := maxResultChars
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n… (%d more bytes)", len(s)-cut)
}

// toolTitle names a call after what it acts on, e.g. "bash: ls -la".
func toolTitle(name string, input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"command", "cmd", "path", "file_path", "url", "pattern", "query"} {
		if detail, ok := m[key].(string); ok && detail != "" {
			detail = strings.SplitN(detail, "\n", 2)[0]
			if r := []rune(detail); len(r) > 120 {
				detail = string(r[:117]) + "..."
			}
			return name + ": " + detail
		}
	}
	return ""
}
