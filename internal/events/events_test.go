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

package events

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestRecorderKeepsNewestFirstAndWraps(t *testing.T) {
	r := NewRecorder(3)
	if got := r.Events(); len(got) != 0 {
		t.Fatalf("empty recorder returned %v", got)
	}
	for _, m := range []string{"a", "b"} {
		r.add(Event{Message: m})
	}
	if got := messages(r.Events()); got != "b,a" {
		t.Errorf("events = %s", got)
	}
	for _, m := range []string{"c", "d", "e"} {
		r.add(Event{Message: m})
	}
	if got := messages(r.Events()); got != "e,d,c" {
		t.Errorf("after wrapping = %s", got)
	}
}

func messages(es []Event) string {
	var ms []string
	for _, e := range es {
		ms = append(ms, e.Message)
	}
	return strings.Join(ms, ",")
}

func TestHandlerRecordsWithAttrsAndPassesOn(t *testing.T) {
	var out bytes.Buffer
	r := NewRecorder(10)
	next := slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn})
	log := slog.New(r.Handler(next, slog.LevelInfo)).With("stitch", "billing")

	log.Debug("too quiet")
	log.Info("connection opened", "a_circuit", "c1", "dial_time", 25*time.Millisecond)
	log.WithGroup("net").Warn("trouble", "error", errors.New("boom"), slog.Group("detail", "code", 7))

	evs := r.Events()
	if len(evs) != 2 {
		t.Fatalf("recorded %d events, want 2 (info and warn)", len(evs))
	}
	open := evs[1]
	if open.Message != "connection opened" || open.Attr("stitch") != "billing" || open.Attr("a_circuit") != "c1" || open.Attr("dial_time") != "25ms" {
		t.Errorf("info event = %+v", open)
	}
	warn := evs[0]
	if warn.Level != slog.LevelWarn || warn.Attr("net.error") != "boom" || warn.Attr("net.detail.code") != "7" || warn.Attr("stitch") != "billing" {
		t.Errorf("warn event = %+v", warn)
	}
	if got := warn.Attr("missing"); got != "" {
		t.Errorf("Attr(missing) = %q", got)
	}

	// The output still follows its own level: only the warning got through.
	if strings.Contains(out.String(), "connection opened") || !strings.Contains(out.String(), "trouble") {
		t.Errorf("output = %q", out.String())
	}
}
