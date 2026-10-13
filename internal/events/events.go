/*
	Copyright 2026 NetFoundry Inc.

	Licensed under the Apache License, Version 2.0 (the "License");
	you may not use this file except in compliance with the License.
	You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

	Unless required by applicable law or agreed to in writing, software
	distributed under the License is distributed on an "AS IS" BASIS,
	WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
	See the License for the specific language governing permissions and
	limitations under the License.
*/

// Package events keeps the most recent log records in memory, so the web
// UI can show what happened without reading log files.
package events

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Event is one log record as kept for display.
type Event struct {
	Time    time.Time
	Level   slog.Level
	Message string
	Attrs   []Attr // in order, groups flattened to "group.key"
}

// Attr is one key and its value rendered as text.
type Attr struct {
	Key, Value string
}

// Attr returns the value of the first attribute named key, or "".
func (e Event) Attr(key string) string {
	for _, a := range e.Attrs {
		if a.Key == key {
			return a.Value
		}
	}
	return ""
}

// Recorder keeps the most recent events in a fixed-size ring.
type Recorder struct {
	mu   sync.Mutex
	ring []Event
	next int  // where the next event goes
	full bool // the ring has wrapped
}

// NewRecorder keeps up to size events.
func NewRecorder(size int) *Recorder {
	return &Recorder{ring: make([]Event, size)}
}

func (r *Recorder) add(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ring[r.next] = e
	r.next = (r.next + 1) % len(r.ring)
	if r.next == 0 {
		r.full = true
	}
}

// Events returns the kept events, newest first.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.next
	if r.full {
		n = len(r.ring)
	}
	out := make([]Event, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, r.ring[(r.next-i+len(r.ring))%len(r.ring)])
	}
	return out
}

// Handler returns a slog.Handler that records events at min and above,
// whatever next's level, and passes every record on to next.
func (r *Recorder) Handler(next slog.Handler, min slog.Level) slog.Handler {
	return &handler{rec: r, next: next, min: min}
}

type handler struct {
	rec    *Recorder
	next   slog.Handler
	min    slog.Level
	attrs  []Attr // from WithAttrs, already flattened
	prefix string // from WithGroup, "a.b."
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.min || h.next.Enabled(ctx, l)
}

func (h *handler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level >= h.min {
		e := Event{Time: rec.Time, Level: rec.Level, Message: rec.Message, Attrs: append([]Attr(nil), h.attrs...)}
		rec.Attrs(func(a slog.Attr) bool {
			e.Attrs = flatten(e.Attrs, h.prefix, a)
			return true
		})
		h.rec.add(e)
	}
	if h.next.Enabled(ctx, rec.Level) {
		return h.next.Handle(ctx, rec)
	}
	return nil
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append([]Attr(nil), h.attrs...)
	for _, a := range as {
		c.attrs = flatten(c.attrs, h.prefix, a)
	}
	c.next = h.next.WithAttrs(as)
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.prefix = h.prefix + name + "."
	c.next = h.next.WithGroup(name)
	return &c
}

func flatten(dst []Attr, prefix string, a slog.Attr) []Attr {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, g := range v.Group() {
			dst = flatten(dst, p, g)
		}
		return dst
	}
	if a.Key == "" {
		return dst
	}
	return append(dst, Attr{Key: prefix + a.Key, Value: v.String()})
}
