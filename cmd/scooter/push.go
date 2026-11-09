package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
)

// priorityInterrupt is /messages' priority: the agent reads it at its next tool call.
const priorityInterrupt = 10

func hasUserChunk(updates []update) bool {
	for _, u := range updates {
		if u["sessionUpdate"] == "user_message_chunk" {
			return true
		}
	}
	return false
}

// attach goes live, then follows the event stream. Live first, so a run
// that arrives as soon as the stream connects already counts as a push;
// the stream's resync on connect catches up on anything before it.
func (s *session) attach() {
	s.goLive(s.ctx)
	go s.follow(s.ctx)
}

// goLive marks the session attached once its replay is out: requests left
// open in history are asked, and a run already going becomes a push.
func (s *session) goLive(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = true
	if s.running && s.turn == nil && !s.pushing {
		s.startPushLocked()
	}
	for _, in := range s.pending {
		s.askLocked(ctx, in)
	}
}

// startPushLocked opens a session/push for a run this client did not prompt.
func (s *session) startPushLocked() {
	s.pushing = true
	// Written now, so the updates that follow arrive inside the push.
	s.pushDone = s.out.start("session/push", map[string]any{"sessionId": s.id})
}

// endPushLocked closes the open push; the client answers it after the end.
func (s *session) endPushLocked() {
	if !s.pushing {
		return
	}
	s.pushing = false
	s.out.notify("session/update", map[string]any{"sessionId": s.id, "update": update{"sessionUpdate": "session_push_end"}})
	wait, ctx := s.pushDone, s.ctx
	s.pushDone = nil
	go func() {
		// A busy client declines the push; its updates still render.
		if err := wait(ctx, nil); err != nil && ctx.Err() == nil {
			log.Printf("session %s: push: %v", s.id, err)
		}
	}()
}

// runEndedLocked notes the end of a run; a push ends with its run unless more is queued.
func (s *session) runEndedLocked(ext bool) {
	if ext {
		return
	}
	s.running = false
	if s.pushing && s.queueEmpty {
		s.endPushLocked()
	}
}

// steer adds a prompt to the turn in flight, as the web UI's interrupt does.
func (s *session) steer(ctx context.Context, text string, images []any) (string, error) {
	s.mu.Lock()
	busy := s.turn != nil || s.running || s.pushing
	s.mu.Unlock()
	if !busy {
		return "promptRequired", nil
	}
	var err error
	if len(images) > 0 {
		err = s.postAguiWith(ctx, nil, promptContent(text, images), "", priorityInterrupt)
	} else {
		err = s.c.call(ctx, "POST", s.path("/messages"), map[string]string{"text": text}, nil)
	}
	if err != nil {
		return "", err
	}
	return "injected", nil
}

// steerSession handles _session/steering.
func (a *agent) steerSession(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string            `json:"sessionId"`
		Prompt    []json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	s := a.get(p.SessionID)
	if s == nil {
		return nil, invalid(fmt.Errorf("unknown session %s", p.SessionID))
	}
	images, err := promptImages(p.Prompt)
	if err != nil {
		return nil, invalid(err)
	}
	if len(images) > 0 && a.opts.self {
		return nil, invalid(errors.New("self-access cannot send images"))
	}
	outcome, err := s.steer(ctx, promptText(p.Prompt), images)
	if err != nil {
		return nil, wrap(err)
	}
	return map[string]any{"outcome": outcome}, nil
}
