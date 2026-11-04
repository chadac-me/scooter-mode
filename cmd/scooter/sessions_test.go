package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestSessionListMatchesTheSidebar(t *testing.T) {
	f, srv := newFake(t)
	f.rows = []map[string]any{
		{"id": "old", "title": "Old", "lastActivityAt": 1000, "status": "suspended"},
		{"id": "new", "title": "New", "lastActivityAt": 2000, "status": "running", "runStartedAt": 1900},
		{"id": "pinned", "title": "Pinned", "lastActivityAt": 500, "starred": true, "awaitingInputAt": 400,
			"links": []any{map[string]any{"source": "github"}, map[string]any{"source": "github"}}},
		{"id": "child", "parentId": "new", "lastActivityAt": 3000},
		{"id": "untitled", "lastActivityAt": 0, "visibility": "private"},
	}
	tc := startBridge(t, srv.URL, agentOptions{})
	var init struct {
		AgentCapabilities struct {
			SessionCapabilities map[string]any
		}
	}
	tc.call(t, "initialize", map[string]any{"protocolVersion": 1}, &init)
	for _, k := range []string{"list", "resume", "close"} {
		if _, ok := init.AgentCapabilities.SessionCapabilities[k]; !ok {
			t.Errorf("sessionCapabilities lacks %s: %v", k, init.AgentCapabilities.SessionCapabilities)
		}
	}

	var res struct {
		Sessions []struct {
			SessionID, Cwd, Title, UpdatedAt string
			Meta                             struct {
				Scooter struct {
					State, Visibility string
					Starred           bool
					Sources           []string
				}
			} `json:"_meta"`
		}
		NextCursor string
	}
	tc.call(t, "session/list", map[string]any{"cwd": "/home/me/proj"}, &res)
	var got []string
	for _, s := range res.Sessions {
		m := s.Meta.Scooter
		got = append(got, fmt.Sprintf("%s|%s|%s|%v|%s|%v", s.SessionID, s.Title, m.State, m.Starred, m.Visibility, m.Sources))
		if s.Cwd != "/home/me/proj" {
			t.Errorf("%s: cwd = %q", s.SessionID, s.Cwd)
		}
	}
	want := []string{
		"pinned|Pinned|awaiting|true|shared|[github]",
		"new|New|running|false|shared|[]",
		"old|Old|suspended|false|shared|[]",
		"untitled|untitled|idle|false|private|[]",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("sessions =\n%v\nwant\n%v", got, want)
	}
	if res.Sessions[1].UpdatedAt != time.UnixMilli(2000).UTC().Format(time.RFC3339Nano) {
		t.Errorf("updatedAt = %q", res.Sessions[1].UpdatedAt)
	}
	if res.Sessions[3].UpdatedAt != "" {
		t.Errorf("updatedAt without activity = %q", res.Sessions[3].UpdatedAt)
	}
	if res.NextCursor != "" {
		t.Errorf("nextCursor = %q on one page", res.NextCursor)
	}
}

func TestSessionListPages(t *testing.T) {
	f, srv := newFake(t)
	for i := range listPageSize + 3 {
		f.rows = append(f.rows, map[string]any{"id": fmt.Sprintf("c%03d", i), "lastActivityAt": 10000 - i})
	}
	tc := startBridge(t, srv.URL, agentOptions{})
	type page struct {
		Sessions   []struct{ SessionID string }
		NextCursor string
	}
	var first, second page
	tc.call(t, "session/list", map[string]any{}, &first)
	if len(first.Sessions) != listPageSize || first.NextCursor == "" {
		t.Fatalf("first page: %d sessions, cursor %q", len(first.Sessions), first.NextCursor)
	}
	tc.call(t, "session/list", map[string]any{"cursor": first.NextCursor}, &second)
	if len(second.Sessions) != 3 || second.NextCursor != "" || second.Sessions[0].SessionID != fmt.Sprintf("c%03d", listPageSize) {
		t.Fatalf("second page = %+v", second)
	}
}

func TestResumeFollowsWithoutReplay(t *testing.T) {
	f, srv := newFake(t)
	run(f, "earlier question",
		map[string]any{"type": "TEXT_MESSAGE_START", "messageId": "a0", "role": "assistant"},
		map[string]any{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a0", "delta": "earlier answer"},
		map[string]any{"type": "TEXT_MESSAGE_END", "messageId": "a0"},
	)
	tc := startBridge(t, srv.URL, agentOptions{})
	tc.call(t, "session/resume", map[string]any{"sessionId": "c1", "cwd": "/tmp"}, nil)
	time.Sleep(100 * time.Millisecond) // let the follower connect
	f.emit(map[string]any{"type": "TEXT_MESSAGE_START", "messageId": "a1", "role": "assistant"})
	f.emit(map[string]any{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a1", "delta": "live"})
	waitFor(t, func() bool { return tc.text("agent_message_chunk") == "live" })
}

func TestCloseStopsFollowing(t *testing.T) {
	f, srv := newFake(t)
	tc := startBridge(t, srv.URL, agentOptions{})
	tc.call(t, "session/resume", map[string]any{"sessionId": "c1"}, nil)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.subs) == 1 })
	tc.call(t, "session/close", map[string]any{"sessionId": "c1"}, nil)
	time.Sleep(100 * time.Millisecond)
	f.emit(map[string]any{"type": "TEXT_MESSAGE_START", "messageId": "a1", "role": "assistant"})
	f.emit(map[string]any{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a1", "delta": "after close"})
	time.Sleep(100 * time.Millisecond)
	if got := tc.text("agent_message_chunk"); got != "" {
		t.Errorf("closed session still streamed %q", got)
	}
	var e struct{}
	if err := tc.rpc.request(context.Background(), "session/prompt", map[string]any{"sessionId": "c1", "prompt": []any{}}, &e); err == nil {
		t.Error("prompt on a closed session succeeded")
	}
}
