package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// turnResult ends a session/prompt call.
type turnResult struct {
	stopReason string
	err        error
}

// turn is a prompt in flight, ended by the run it starts.
type turn struct {
	started   bool
	cancelled bool
	done      chan turnResult
}

// session forwards one Scooter conversation as an ACP session.
type session struct {
	id  string
	c   *client
	out *rpc

	mu         sync.Mutex
	tr         *translator
	held       string // checksum through the last applied event
	applied    int    // events applied, an index into /history
	synced     bool   // a live frame has chained onto held since (re)connecting
	queueEmpty bool
	turn       *turn
	pending    map[string]interrupt // unanswered permission requests, by tool call
	asked      map[string]bool      // permission requests already sent to the client
	prefix     string               // prepended to the first prompt
	model      string               // the picked model, sent with each prompt
	convModel  string               // the model the conversation was created with
	live       bool                 // attached: runs from elsewhere arrive as session/push
	running    bool                 // an agent run is in flight, whoever started it
	pushing    bool                 // a session/push is open for a run from elsewhere
	pushDone   func(context.Context, any) error

	approveURL string      // the web UI page that answers requests
	noAnswer   bool        // the server refuses permission answers from us
	forms      bool        // the client renders elicitation forms
	following  atomic.Bool // one stream per conversation, however often it is opened

	// ctx ends with session/close, stopping the stream and pending requests.
	ctx  context.Context
	stop context.CancelFunc
}

type history struct {
	Events   []json.RawMessage `json:"events"`
	Checksum string            `json:"checksum"`
}

type frame struct {
	Kind         string          `json:"kind"`
	Event        json.RawMessage `json:"event"`
	Checksum     string          `json:"checksum"`
	PrevChecksum string          `json:"prevChecksum"`
}

func newSession(id string, c *client, out *rpc) *session {
	return &session{
		id: id, c: c, out: out, tr: newTranslator(), queueEmpty: true,
		pending: map[string]interrupt{}, asked: map[string]bool{},
	}
}

func (s *session) path(suffix string) string {
	return "/conversations/" + url.PathEscape(s.id) + suffix
}

// load applies history, emitting its updates when send is set.
func (s *session) load(ctx context.Context, send bool) error {
	var h history
	if err := s.c.call(ctx, "GET", s.path("/history"), nil, &h); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyNew(ctx, h, send)
	return nil
}

// applyNew applies the history events past s.applied. Caller holds mu.
func (s *session) applyNew(ctx context.Context, h history, send bool) {
	for _, raw := range h.Events[min(s.applied, len(h.Events)):] {
		s.applyLocked(ctx, raw, send, true)
	}
	s.applied = len(h.Events)
	s.held = h.Checksum
}

// applyLocked forwards an event and drives the turn; holds mu.
func (s *session) applyLocked(ctx context.Context, raw json.RawMessage, send, replay bool) {
	var ev aguiEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		log.Printf("session %s: bad event: %v", s.id, err)
		return
	}
	updates, sig := s.tr.apply(ev)
	ext := strings.HasPrefix(ev.RunID, "ext-")
	if send && s.live && s.turn == nil && !s.pushing && ((sig == sigRunStarted && !ext) || hasUserChunk(updates)) {
		// A run from the web UI or elsewhere; its prompt comes just before it starts.
		s.startPushLocked()
	}
	if send {
		for _, u := range updates {
			s.resolveImages(ctx, u)
			s.out.notify("session/update", map[string]any{"sessionId": s.id, "update": u})
		}
	}
	switch sig {
	case sigQueue:
		var items []json.RawMessage
		if err := json.Unmarshal(ev.Items, &items); err != nil {
			log.Printf("session %s: queue: %v", s.id, err)
			break
		}
		s.queueEmpty = len(items) == 0
	case sigResolved:
		delete(s.pending, ev.ToolCallID)
	case sigInterrupt:
		for _, in := range ev.Outcome.Interrupts {
			if len(in.Metadata.Options) == 0 {
				continue
			}
			s.pending[in.ID] = in
			if !replay {
				s.askLocked(ctx, in)
			}
		}
	case sigRunStarted:
		if !ext {
			s.running = true
		}
		if s.turn != nil {
			s.turn.started = true
		}
	case sigRunEnded:
		s.runEndedLocked(ext)
		if t := s.turn; t != nil && t.started && s.queueEmpty {
			reason := "end_turn"
			if t.cancelled || ev.Cancelled {
				reason = "cancelled"
			}
			s.finishLocked(turnResult{stopReason: reason})
		}
	case sigRunError:
		s.runEndedLocked(ext)
		if t := s.turn; t != nil && t.started && s.queueEmpty {
			s.finishLocked(turnResult{err: errors.New(ev.Message)})
		}
	}
}

func (s *session) finishLocked(r turnResult) {
	s.turn.done <- r
	s.turn = nil
	s.tr.echoUser = true
}

// askLocked puts a pending request to the client; holds mu.
func (s *session) askLocked(ctx context.Context, in interrupt) {
	if s.asked[in.ID] {
		return
	}
	s.asked[in.ID] = true
	qs := questionsOf(in.Metadata.Data)
	isQuestions := in.Metadata.Kind == kindQuestions && len(qs) > 0
	switch {
	case s.noAnswer || (isQuestions && !s.forms):
		s.pointAtWebUI(in, qs)
	case isQuestions:
		go s.askForm(ctx, in, qs)
	default:
		go s.askPermission(ctx, in)
	}
}

// pointAtWebUI shows a request this client can't answer; qs may be nil.
func (s *session) pointAtWebUI(in interrupt, qs []question) {
	note := "\n\n> Waiting for approval: " + in.Message + ". Answer it"
	if len(qs) > 0 {
		note = "\n\n> The agent asks:" + strings.ReplaceAll(questionsText(qs), "\n", "\n> ") + "\n>\n> Answer it"
	}
	note += " in the Scooter web UI"
	if s.approveURL != "" {
		note += ": " + s.approveURL
	}
	s.out.notify("session/update", map[string]any{"sessionId": s.id, "update": textChunk("agent_message_chunk", note+".\n")})
}

// askPermission asks for one of the request's options.
func (s *session) askPermission(ctx context.Context, in interrupt) {
	toolCallID := in.ToolCallID
	if toolCallID == "" {
		toolCallID = in.ID
	}
	title, body := describe(in)
	call := map[string]any{"toolCallId": toolCallID, "title": title}
	if body != "" {
		call["content"] = []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": body}}}
	}
	var res struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	err := s.out.request(ctx, "session/request_permission", map[string]any{
		"sessionId": s.id,
		"toolCall":  call,
		"options":   in.Metadata.Options,
	}, &res)
	if err != nil || res.Outcome.Outcome != "selected" {
		return
	}
	s.resume(ctx, in.ID, res.Outcome.OptionID, nil)
}

// askForm asks AskUserQuestion's questions as an ACP form.
func (s *session) askForm(ctx context.Context, in interrupt, qs []question) {
	var res struct {
		Action  string         `json:"action"`
		Content map[string]any `json:"content"`
	}
	params := map[string]any{
		"sessionId":       s.id,
		"mode":            "form",
		"message":         in.Message,
		"requestedSchema": questionSchema(qs),
	}
	if in.ToolCallID != "" {
		params["toolCallId"] = in.ToolCallID
	}
	if err := s.out.request(ctx, "elicitation/create", params, &res); err != nil {
		log.Printf("session %s: ask questions: %v", s.id, err)
		if ctx.Err() == nil {
			s.pointAtWebUI(in, qs)
		}
		return
	}
	answers, ok := formAnswers(qs, res.Content)
	if res.Action != "accept" || !ok {
		s.resume(ctx, in.ID, "", nil)
		return
	}
	// The answer option carries the result; Scooter offers exactly one.
	optionID := "answer"
	if len(in.Metadata.Options) > 0 {
		optionID = in.Metadata.Options[0].OptionID
	}
	s.resume(ctx, in.ID, optionID, map[string]any{"answers": answers})
}

// resume answers an interrupt as the web UI does; no optionID dismisses it.
func (s *session) resume(ctx context.Context, id, optionID string, result map[string]any) {
	entry := map[string]any{"interruptId": id, "status": "cancelled"}
	if optionID != "" {
		payload := map[string]any{"optionId": optionID}
		if result != nil {
			payload["result"] = result
		}
		entry["status"], entry["payload"] = "resolved", payload
	}
	resp, err := s.c.do(ctx, "POST", "/agui", map[string]any{"threadId": s.id, "resume": []any{entry}})
	if err != nil {
		log.Printf("session %s: answer %s: %v", s.id, id, err)
		return
	}
	// The resumed run streams back through the integrity log instead.
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		log.Printf("session %s: answer %s: %s", s.id, id, resp.Status)
	}
}

// follow tails the integrity stream, resyncing after gaps or drops.
func (s *session) follow(ctx context.Context) {
	if s.following.CompareAndSwap(false, true) {
		s.tail(ctx)
	}
}

func (s *session) tail(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := s.c.stream(ctx, s.path("/events.integrity"), func() {
			backoff = time.Second
			// Catch up on anything appended before the stream went live.
			if err := s.resync(ctx); err != nil {
				log.Printf("session %s: resync: %v", s.id, err)
			}
		}, func(data []byte) { s.onFrame(ctx, data) })
		if ctx.Err() != nil {
			return
		}
		log.Printf("session %s: stream ended: %v; reconnecting in %s", s.id, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (s *session) resync(ctx context.Context) error {
	var h history
	if err := s.c.call(ctx, "GET", s.path("/history"), nil, &h); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyNew(ctx, h, true)
	s.synced = false
	// Requests that landed in the gap still need asking; asked dedups.
	for _, in := range s.pending {
		s.askLocked(ctx, in)
	}
	return nil
}

func (s *session) onFrame(ctx context.Context, data []byte) {
	var f frame
	if json.Unmarshal(data, &f) != nil || f.Kind != "event" || f.Checksum == "" {
		// Unchecksummed frames are the stream's own replay; history covers them.
		return
	}
	s.mu.Lock()
	switch {
	case f.PrevChecksum == s.held:
		s.applyLocked(ctx, f.Event, true, false)
		s.held = f.Checksum
		s.applied++
		s.synced = true
		s.mu.Unlock()
	case !s.synced:
		// Already in the history fetched at connect.
		s.mu.Unlock()
	default:
		s.mu.Unlock()
		if err := s.resync(ctx); err != nil {
			log.Printf("session %s: resync after gap: %v", s.id, err)
		}
	}
}

// prompt sends text and waits for its turn to end.
func (s *session) prompt(ctx context.Context, text string, images []any) (string, error) {
	s.mu.Lock()
	if s.turn != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("a prompt is already running in %s", s.id)
	}
	if s.pushing {
		// The client queues prompts during a push, so this one follows it.
		s.endPushLocked()
	}
	t := &turn{done: make(chan turnResult, 1)}
	s.turn = t
	s.tr.echoUser = false
	if s.prefix != "" {
		text = s.prefix + "\n\n" + text
		s.prefix = ""
	}
	model := s.model
	s.mu.Unlock()

	var err error
	if model != "" || len(images) > 0 {
		// Only /agui carries a model pick or images.
		err = s.postAgui(ctx, t, promptContent(text, images), model)
	} else {
		err = s.c.call(ctx, "POST", s.path("/messages"), map[string]string{"text": text}, nil)
	}
	if err != nil {
		s.mu.Lock()
		if s.turn == t {
			s.turn = nil
			s.tr.echoUser = true
		}
		s.mu.Unlock()
		return "", err
	}
	select {
	case r := <-t.done:
		return r.stopReason, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *session) cancel(ctx context.Context) {
	s.mu.Lock()
	if t := s.turn; t != nil {
		t.cancelled = true
		// No run started, so no run end will close it.
		if !t.started {
			s.finishLocked(turnResult{stopReason: "cancelled"})
		}
	}
	s.mu.Unlock()
	if err := s.c.call(ctx, "POST", s.path("/cancel"), nil, nil); err != nil {
		log.Printf("session %s: cancel: %v", s.id, err)
	}
}

// currentModel is the picked model, else the conversation's own.
func (s *session) currentModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.model != "" {
		return s.model
	}
	return s.convModel
}
