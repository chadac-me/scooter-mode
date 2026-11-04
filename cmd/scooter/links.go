package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
)

// link is one resource linked to a conversation.
type link struct {
	Source       string `json:"source"`
	ResourceType string `json:"resourceType"`
	URL          string `json:"url"`
	Title        string `json:"title"`
}

func (l link) key() string {
	return l.Source + "\x00" + l.ResourceType + "\x00" + l.URL + "\x00" + l.Title
}

// label names l as the web UI's linked-resources panel does.
func (l link) label() string {
	if l.Title != "" {
		return l.Source + ": " + l.Title
	}
	return l.Source + ": " + strings.ReplaceAll(l.ResourceType, "_", " ")
}

// linkFrame is one /conversations/events frame.
type linkFrame struct {
	Kind          string `json:"kind"`
	Conversation  *linkRow
	Conversations []linkRow `json:"conversations"`
}

type linkRow struct {
	ID    string `json:"id"`
	Links []link `json:"links"`
}

// watchLinks announces links added to open conversations, from the
// conversation list stream, until ctx ends.
func (a *agent) watchLinks(ctx context.Context) {
	known := map[string]map[string]bool{}
	seen := func(r linkRow) {
		// Every row sets a baseline, so a session opened later hears only what is new.
		s := a.get(r.ID)
		keys := map[string]bool{}
		var added []link
		for _, l := range r.Links {
			keys[l.key()] = true
			if before, ok := known[r.ID]; ok && !before[l.key()] {
				added = append(added, l)
			}
		}
		known[r.ID] = keys
		if s != nil && len(added) > 0 {
			s.announceLinks(added)
		}
	}
	backoff := time.Second
	for ctx.Err() == nil {
		err := a.c.stream(ctx, "/conversations/events", func() { backoff = time.Second }, func(data []byte) {
			var f linkFrame
			if json.Unmarshal(data, &f) != nil {
				return
			}
			switch {
			case f.Kind == "snapshot":
				for _, r := range f.Conversations {
					seen(r)
				}
			case f.Kind == "upsert" && f.Conversation != nil:
				seen(*f.Conversation)
			}
		})
		if ctx.Err() != nil {
			return
		}
		log.Printf("conversation list stream: %v; reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// announceLinks notes new links in the shell, as the web UI's panel lists them.
func (s *session) announceLinks(added []link) {
	var b strings.Builder
	b.WriteString("\n\n> **Linked:**")
	for _, l := range added {
		if l.URL != "" {
			fmt.Fprintf(&b, " [%s](%s)", l.label(), l.URL)
		} else {
			b.WriteString(" " + l.label())
		}
	}
	s.out.notify("session/update", map[string]any{"sessionId": s.id, "update": textChunk("agent_message_chunk", b.String()+"\n\n")})
}

// serviceCommand is a scooter-service call in a shell command.
var serviceCommand = regexp.MustCompile(`\bscooter-service\s+(start|stop|restart)\s+([\w.-]+)`)

// serviceNotices points at the web services a tool call started or stopped.
func serviceNotices(input any) []update {
	m, _ := input.(map[string]any)
	command, _ := m["command"].(string)
	if command == "" {
		command, _ = m["cmd"].(string)
	}
	var out []update
	for _, match := range serviceCommand.FindAllStringSubmatch(command, -1) {
		verb := map[string]string{"start": "started", "stop": "stopped", "restart": "restarted"}[match[1]]
		out = append(out, textChunk("agent_message_chunk", fmt.Sprintf("\n\n> Web service `%s` %s (`M-x scooter-web-services`)\n\n", match[2], verb)))
	}
	return out
}
