package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
)

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

const (
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeAuthRequired   = -32000
)

type rpcMsg struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

// handlerFunc answers a request; after runs once the response is sent.
type handlerFunc func(ctx context.Context, method string, params json.RawMessage) (result any, after func(), err error)

// notifyFunc receives a client notification.
type notifyFunc func(method string, params json.RawMessage)

// rpc is a newline-delimited JSON-RPC 2.0 peer.
type rpc struct {
	wmu     sync.Mutex
	w       io.Writer
	nextID  atomic.Int64
	pmu     sync.Mutex
	pending map[int64]chan rpcMsg
}

func newRPC(w io.Writer) *rpc { return &rpc{w: w, pending: map[int64]chan rpcMsg{}} }

func (p *rpc) write(m rpcMsg) {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		log.Printf("rpc: marshal: %v", err)
		return
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if _, err := p.w.Write(append(b, '\n')); err != nil {
		log.Printf("rpc: write: %v", err)
	}
}

func (p *rpc) notify(method string, params any) {
	b, _ := json.Marshal(params)
	p.write(rpcMsg{Method: method, Params: b})
}

// request sends a request and waits for its result.
func (p *rpc) request(ctx context.Context, method string, params any, out any) error {
	return p.start(method, params)(ctx, out)
}

// start writes a request now and returns a wait for its result, so the
// caller can order later messages after it without blocking on the answer.
func (p *rpc) start(method string, params any) func(ctx context.Context, out any) error {
	id := p.nextID.Add(1)
	ch := make(chan rpcMsg, 1)
	p.pmu.Lock()
	p.pending[id] = ch
	p.pmu.Unlock()
	b, _ := json.Marshal(params)
	raw := json.RawMessage(fmt.Sprint(id))
	p.write(rpcMsg{ID: &raw, Method: method, Params: b})
	return func(ctx context.Context, out any) error {
		defer func() {
			p.pmu.Lock()
			delete(p.pending, id)
			p.pmu.Unlock()
		}()
		select {
		case m := <-ch:
			if m.Error != nil {
				return m.Error
			}
			if out != nil {
				return json.Unmarshal(m.Result, out)
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// serve reads messages until r ends; requests are handled concurrently.
func (p *rpc) serve(ctx context.Context, r io.Reader, handle handlerFunc, notify notifyFunc) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var wg sync.WaitGroup
	defer wg.Wait()
	for sc.Scan() {
		var m rpcMsg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			log.Printf("rpc: bad message: %v", err)
			continue
		}
		switch {
		case m.Method != "" && m.ID != nil:
			wg.Add(1)
			go func(m rpcMsg) {
				defer wg.Done()
				p.answer(ctx, m, handle)
			}(m)
		case m.Method != "":
			notify(m.Method, m.Params)
		case m.ID != nil:
			var id int64
			if json.Unmarshal(*m.ID, &id) == nil {
				p.pmu.Lock()
				ch := p.pending[id]
				p.pmu.Unlock()
				if ch != nil {
					ch <- m
				}
			}
		}
	}
	return sc.Err()
}

func (p *rpc) answer(ctx context.Context, m rpcMsg, handle handlerFunc) {
	result, after, err := handle(ctx, m.Method, m.Params)
	if err != nil {
		re, ok := err.(*rpcError)
		if !ok {
			re = &rpcError{Code: codeInternal, Message: err.Error()}
		}
		p.write(rpcMsg{ID: m.ID, Error: re})
		return
	}
	if result == nil {
		result = struct{}{}
	}
	b, _ := json.Marshal(result)
	p.write(rpcMsg{ID: m.ID, Result: b})
	if after != nil {
		after()
	}
}
