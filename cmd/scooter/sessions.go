package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"
)

// listPageSize is how many conversations one session/list page holds.
const listPageSize = 50

// conversationRow is the slice of a GET /conversations row session/list reads.
type conversationRow struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	ParentID        string   `json:"parentId"`
	Status          string   `json:"status"`
	Visibility      string   `json:"visibility"`
	Model           string   `json:"model"`
	Starred         bool     `json:"starred"`
	LastActivityAt  float64  `json:"lastActivityAt"`
	RunStartedAt    float64  `json:"runStartedAt"`
	AwaitingInputAt float64  `json:"awaitingInputAt"`
	Sources         []string `json:"sources"`
	Links           []struct {
		Source string `json:"source"`
	} `json:"links"`
}

// state ranks the row as scooter-list-status does, so both views agree.
func (r conversationRow) state() string {
	switch {
	case r.Status == "failed":
		return "failed"
	case r.AwaitingInputAt != 0:
		return "awaiting"
	case r.Status == "suspended":
		return "suspended"
	case r.RunStartedAt != 0:
		return "running"
	}
	return "idle"
}

// sources names the services linked to the conversation, once each.
func (r conversationRow) sources() []string {
	if len(r.Sources) > 0 {
		return r.Sources
	}
	var out []string
	for _, l := range r.Links {
		if l.Source != "" && !slices.Contains(out, l.Source) {
			out = append(out, l.Source)
		}
	}
	return out
}

// sessionInfo is the row as an ACP SessionInfo; Scooter's own fields go under _meta.
func (r conversationRow) sessionInfo(cwd string) map[string]any {
	title := r.Title
	if title == "" {
		title = r.ID
	}
	visibility := r.Visibility
	if visibility == "" {
		visibility = "shared"
	}
	sources := r.sources()
	if sources == nil {
		sources = []string{}
	}
	info := map[string]any{
		"sessionId": r.ID,
		// Conversations live in a sandbox, not a local directory, so echo the client's.
		"cwd":   cwd,
		"title": title,
		"_meta": map[string]any{"scooter": map[string]any{
			"state": r.state(), "starred": r.Starred, "status": r.Status,
			"visibility": visibility, "model": r.Model, "sources": sources,
		}},
	}
	if r.LastActivityAt > 0 {
		info["updatedAt"] = time.UnixMilli(int64(r.LastActivityAt)).UTC().Format(time.RFC3339Nano)
	}
	return info
}

// listSessions pages through the caller's top-level conversations, starred
// first and then most recently active, as the web UI's sidebar orders them.
func (a *agent) listSessions(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Cwd    string `json:"cwd"`
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	if a.opts.self {
		// Self-access reaches only its own conversation.
		return map[string]any{"sessions": []any{}}, nil
	}
	offset := 0
	if p.Cursor != "" {
		n, err := strconv.Atoi(p.Cursor)
		if err != nil || n < 0 {
			return nil, invalid(fmt.Errorf("bad cursor %q", p.Cursor))
		}
		offset = n
	}
	var rows []conversationRow
	if err := a.c.call(ctx, "GET", "/conversations", nil, &rows); err != nil {
		return nil, wrap(err)
	}
	rows = slices.DeleteFunc(rows, func(r conversationRow) bool { return r.ParentID != "" })
	slices.SortStableFunc(rows, func(x, y conversationRow) int {
		if x.Starred != y.Starred {
			if x.Starred {
				return -1
			}
			return 1
		}
		switch {
		case x.LastActivityAt > y.LastActivityAt:
			return -1
		case x.LastActivityAt < y.LastActivityAt:
			return 1
		}
		return 0
	})
	cwd := p.Cwd
	if cwd == "" {
		cwd = "/workspace"
	}
	sessions := []any{}
	end := min(offset+listPageSize, len(rows))
	for _, r := range rows[min(offset, len(rows)):end] {
		sessions = append(sessions, r.sessionInfo(cwd))
	}
	res := map[string]any{"sessions": sessions}
	if end < len(rows) {
		res["nextCursor"] = strconv.Itoa(end)
	}
	return res, nil
}

// resumeSession attaches to a conversation without replaying its history.
func (a *agent) resumeSession(ctx context.Context, id string) (any, func(), error) {
	if a.opts.self {
		if id != a.opts.conversation {
			return nil, nil, invalid(fmt.Errorf("self-access reaches only conversation %s", a.opts.conversation))
		}
	} else if err := a.c.call(ctx, "GET", "/conversations/"+url.PathEscape(id), nil, nil); err != nil {
		return nil, nil, wrap(err)
	}
	s := a.open(id)
	if err := s.load(ctx, false); err != nil {
		return nil, nil, wrap(err)
	}
	go s.follow(s.ctx)
	return map[string]any{}, func() { s.replayPending(s.ctx) }, nil
}

// closeSession stops following a conversation and ends its turn, if any.
func (a *agent) closeSession(id string) {
	a.mu.Lock()
	s := a.sessions[id]
	delete(a.sessions, id)
	a.mu.Unlock()
	if s == nil {
		return
	}
	s.stop()
	s.mu.Lock()
	if s.turn != nil {
		s.finishLocked(turnResult{stopReason: "cancelled"})
	}
	s.mu.Unlock()
}
