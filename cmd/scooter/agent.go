package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"strings"
	"sync"
)

type agentOptions struct {
	conversation string // attach the first session/new to this id
	model        string
	project      bool // describe the cwd's git checkout in the first prompt
	// self: the router's sandbox self-access, which reaches one conversation
	// and may not answer permissions; approvals point at approveURL instead.
	self       bool
	approveURL string
}

// agent maps ACP sessions onto Scooter conversations.
type agent struct {
	c    *client
	out  *rpc
	opts agentOptions
	ctx  context.Context

	mu       sync.Mutex
	sessions map[string]*session
	attached bool
	forms    bool // the client advertised elicitation forms
}

func newAgent(ctx context.Context, c *client, out *rpc, opts agentOptions) *agent {
	return &agent{c: c, out: out, opts: opts, ctx: ctx, sessions: map[string]*session{}}
}

func invalid(err error) error { return &rpcError{Code: codeInvalidParams, Message: err.Error()} }

func wrap(err error) error {
	if errors.Is(err, errAuthWall) {
		return &rpcError{Code: codeAuthRequired, Message: err.Error()}
	}
	return err
}

func (a *agent) handle(ctx context.Context, method string, params json.RawMessage) (any, func(), error) {
	switch method {
	case "initialize":
		var p struct {
			ClientCapabilities struct {
				Elicitation map[string]json.RawMessage `json:"elicitation"`
			} `json:"clientCapabilities"`
		}
		_ = json.Unmarshal(params, &p)
		_, forms := p.ClientCapabilities.Elicitation["form"]
		a.mu.Lock()
		a.forms = forms
		a.mu.Unlock()
		if err := a.probe(ctx); err != nil {
			return nil, nil, wrap(err)
		}
		return map[string]any{
			"protocolVersion": 1,
			"agentCapabilities": map[string]any{
				"loadSession":         true,
				"promptCapabilities":  map[string]any{"image": false, "audio": false, "embeddedContext": true},
				"sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}, "close": map[string]any{}},
			},
			"authMethods": []any{},
		}, nil, nil
	case "session/new":
		var p struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, nil, invalid(err)
		}
		return a.newSession(ctx, p.Cwd)
	case "session/load":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
			return nil, nil, invalid(fmt.Errorf("sessionId required"))
		}
		s := a.open(p.SessionID)
		if err := s.load(ctx, true); err != nil {
			return nil, nil, wrap(err)
		}
		go s.follow(s.ctx)
		return a.sessionResult(ctx, s, false), func() { s.replayPending(s.ctx) }, nil
	case "session/list":
		res, err := a.listSessions(ctx, params)
		return res, nil, err
	case "session/resume", "session/close":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
			return nil, nil, invalid(fmt.Errorf("sessionId required"))
		}
		if method == "session/close" {
			a.closeSession(p.SessionID)
			return map[string]any{}, nil, nil
		}
		return a.resumeSession(ctx, p.SessionID)
	case "session/set_config_option", "session/set_model":
		res, err := a.setModel(ctx, method, params)
		return res, nil, err
	case "session/prompt":
		var p struct {
			SessionID string            `json:"sessionId"`
			Prompt    []json.RawMessage `json:"prompt"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, nil, invalid(err)
		}
		s := a.get(p.SessionID)
		if s == nil {
			return nil, nil, invalid(fmt.Errorf("unknown session %s", p.SessionID))
		}
		reason, err := s.prompt(ctx, promptText(p.Prompt))
		if err != nil {
			return nil, nil, wrap(err)
		}
		return map[string]any{"stopReason": reason}, nil, nil
	}
	return nil, nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
}

func (a *agent) notify(method string, params json.RawMessage) {
	if method != "session/cancel" {
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(params, &p) == nil {
		if s := a.get(p.SessionID); s != nil {
			go s.cancel(a.ctx)
		}
	}
}

func (a *agent) newSession(ctx context.Context, cwd string) (any, func(), error) {
	a.mu.Lock()
	// Self-access cannot create conversations, so every session is this one.
	attach := a.opts.self || (a.opts.conversation != "" && !a.attached)
	a.attached = a.attached || attach
	a.mu.Unlock()

	if attach {
		if !a.opts.self {
			if err := a.c.call(ctx, "GET", "/conversations/"+url.PathEscape(a.opts.conversation), nil, nil); err != nil {
				return nil, nil, wrap(err)
			}
		}
		s := a.open(a.opts.conversation)
		// Replay after responding, so the client knows the id.
		return a.sessionResult(ctx, s, true), func() {
			if err := s.load(s.ctx, true); err != nil {
				log.Printf("session %s: load: %v", s.id, err)
			}
			go s.follow(s.ctx)
			s.replayPending(s.ctx)
		}, nil
	}

	body := map[string]any{}
	if a.opts.model != "" {
		body["model"] = a.opts.model
	}
	var conv struct {
		ID string `json:"id"`
	}
	if err := a.c.call(ctx, "POST", "/conversations", body, &conv); err != nil {
		return nil, nil, wrap(err)
	}
	s := a.open(conv.ID)
	s.mu.Lock()
	s.convModel = a.opts.model
	s.mu.Unlock()
	if a.opts.project {
		s.prefix = projectContext(ctx, cwd)
	}
	go s.follow(s.ctx)
	return a.sessionResult(ctx, s, true), nil, nil
}

// probe checks the server and credentials before any session.
func (a *agent) probe(ctx context.Context) error {
	if a.opts.self {
		return a.c.call(ctx, "GET", "/conversations/"+url.PathEscape(a.opts.conversation)+"/history", nil, nil)
	}
	return a.c.call(ctx, "GET", "/whoami", nil, nil)
}

func (a *agent) open(id string) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.sessions[id]; s != nil {
		return s
	}
	s := newSession(id, a.c, a.out)
	s.ctx, s.stop = context.WithCancel(a.ctx)
	s.forms = a.forms
	s.approveURL = a.c.base + "/chat/" + url.PathEscape(id)
	if a.opts.self {
		s.approveURL = a.opts.approveURL
		s.noAnswer = true
	}
	a.sessions[id] = s
	return s
}

func (a *agent) get(id string) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

// promptText flattens ACP content blocks into one message.
func promptText(blocks []json.RawMessage) string {
	var parts []string
	for _, raw := range blocks {
		var b struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			URI      string `json:"uri"`
			Name     string `json:"name"`
			Resource struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"resource"`
		}
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "resource":
			parts = append(parts, fmt.Sprintf("%s:\n```\n%s\n```", b.Resource.URI, b.Resource.Text))
		case "resource_link":
			parts = append(parts, fmt.Sprintf("[%s](%s)", b.Name, b.URI))
		}
	}
	return strings.Join(parts, "\n\n")
}

// projectContext names the git remote and branch to clone.
func projectContext(ctx context.Context, cwd string) string {
	git := func(args ...string) string {
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	remote := git("remote", "get-url", "origin")
	if remote == "" {
		return ""
	}
	branch := git("rev-parse", "--abbrev-ref", "HEAD")
	return fmt.Sprintf("I'm working in %s on branch %s. Clone it into /workspace and check out that branch.", remote, branch)
}
