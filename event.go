package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Event is the part of a Lark v2 event the gateway looks at. The full body is
// kept as it arrived and handed to the route's command unchanged.
type Event struct {
	ID   string
	Type string
	Body []byte
	doc  map[string]any
}

func parseEvent(body []byte) (*Event, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	ev := &Event{Body: body, doc: doc}
	ev.ID, _ = lookup(doc, "header.event_id")
	ev.Type, _ = lookup(doc, "header.event_type")
	if ev.ID == "" || ev.Type == "" {
		return nil, fmt.Errorf("no header.event_id / header.event_type (only v2 events are routed)")
	}
	return ev, nil
}

// Matches reports whether the event is one the route wants: the right event
// type, and every match path holding one of its listed values.
func (r *RouteCfg) Matches(ev *Event) bool {
	if r.Event == "" || r.Event != ev.Type {
		return false
	}
	for path, allowed := range r.Match {
		got, ok := lookup(ev.doc, path)
		if !ok || !contains(allowed, got) {
			return false
		}
	}
	return true
}

// lookup follows a dotted path ("event.message.chat_id") and returns the
// value as text. Numbers and booleans come back as JSON would print them.
func lookup(doc map[string]any, path string) (string, bool) {
	var cur any = doc
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		if cur, ok = m[part]; !ok {
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case nil:
		return "", false
	default:
		b, _ := json.Marshal(v)
		return string(b), true
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
