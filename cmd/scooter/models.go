package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"slices"
)

// modelCatalog is the slice of GET /models the picker reads.
type modelCatalog struct {
	Default   string   `json:"default"`
	Available []string `json:"available"`
}

// configOptions is the session's model picker, or none when the server lists no models.
func (a *agent) configOptions(ctx context.Context, s *session) []any {
	var cat modelCatalog
	if err := a.c.call(ctx, "GET", "/models?conversation="+url.QueryEscape(s.id), nil, &cat); err != nil {
		log.Printf("session %s: models: %v", s.id, err)
		return []any{}
	}
	var ids []string
	for _, id := range append([]string{cat.Default}, cat.Available...) {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return []any{}
	}
	current := s.currentModel()
	if current == "" {
		current = ids[0]
	}
	var opts []any
	for _, id := range ids {
		name := id
		if id == cat.Default {
			name += " (default)"
		}
		opts = append(opts, map[string]any{"value": id, "name": name})
	}
	return []any{map[string]any{
		"id": "model", "name": "Model", "category": "model", "type": "select",
		"currentValue": current, "options": opts,
	}}
}

// setModel handles session/set_config_option and session/set_model.
func (a *agent) setModel(ctx context.Context, method string, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
		ConfigID  string `json:"configId"`
		Value     string `json:"value"`
		ModelID   string `json:"modelId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	s := a.get(p.SessionID)
	if s == nil {
		return nil, invalid(fmt.Errorf("unknown session %s", p.SessionID))
	}
	if a.opts.self {
		return nil, invalid(errors.New("self-access cannot pick a model"))
	}
	model := p.ModelID
	if method == "session/set_config_option" {
		if p.ConfigID != "model" {
			return nil, invalid(fmt.Errorf("unknown config option %s", p.ConfigID))
		}
		model = p.Value
	}
	if model == "" {
		return nil, invalid(errors.New("model required"))
	}
	s.mu.Lock()
	s.model = model
	s.mu.Unlock()
	if method == "session/set_model" {
		return map[string]any{}, nil
	}
	return map[string]any{"configOptions": a.configOptions(ctx, s)}, nil
}

// conversationModel reads the model the conversation was created with.
func (a *agent) conversationModel(ctx context.Context, id string) string {
	var conv struct {
		Model string `json:"model"`
	}
	if err := a.c.call(ctx, "GET", "/conversations/"+url.PathEscape(id), nil, &conv); err != nil {
		return ""
	}
	return conv.Model
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// postAgui starts a run as the web UI does, carrying the picked model and any
// content parts. The run streams back through the integrity log; this only
// watches the response for a failure before the run starts.
func (s *session) postAgui(ctx context.Context, t *turn, content any, model string) error {
	body := map[string]any{
		"threadId": s.id, "runId": newID(),
		"messages": []any{map[string]any{"id": newID(), "role": "user", "content": content}},
	}
	var extra map[string]string
	if model != "" {
		extra = map[string]string{"X-Agent-Model": model}
	}
	resp, err := s.c.doWith(ctx, "POST", "/agui", body, extra)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return fmt.Errorf("POST /agui: %s", resp.Status)
	}
	go func() {
		defer resp.Body.Close()
		_ = sseEach(resp.Body, func(data []byte) bool {
			var ev aguiEvent
			if json.Unmarshal(data, &ev) != nil {
				return true
			}
			switch ev.Type {
			case "RUN_STARTED":
				// The integrity stream carries the rest.
				return false
			case "RUN_ERROR":
				s.mu.Lock()
				if s.turn == t && !t.started {
					s.finishLocked(turnResult{err: errors.New(ev.Message)})
				}
				s.mu.Unlock()
				return false
			}
			return true
		})
	}()
	return nil
}

// sessionResult answers session/new, load and resume, with the model picker.
func (a *agent) sessionResult(ctx context.Context, s *session, withID bool) map[string]any {
	res := map[string]any{}
	if withID {
		res["sessionId"] = s.id
	}
	if a.opts.self {
		return res
	}
	if model := a.conversationModel(ctx, s.id); model != "" {
		s.mu.Lock()
		s.convModel = model
		s.mu.Unlock()
	}
	res["configOptions"] = a.configOptions(ctx, s)
	return res
}
