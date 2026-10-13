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

package stitch

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openziti/sdk-golang/ziti/edge"
)

type fakeListener struct {
	mu      sync.Mutex
	updates []edge.Precedence
}

func (f *fakeListener) UpdatePrecedence(p edge.Precedence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, p)
	return nil
}

func (f *fakeListener) history() []edge.Precedence {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]edge.Precedence(nil), f.updates...)
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func neverBack(context.Context) bool { return false }

func TestGateLowersOnceAndRestoresOnSuccess(t *testing.T) {
	l := &fakeListener{}
	g := newGate(l, edge.PrecedenceRequired, neverBack, discard)
	defer g.stop()

	g.ok() // nothing to restore yet
	g.failed()
	g.failed() // already down
	g.ok()
	g.ok()

	want := []edge.Precedence{edge.PrecedenceFailed, edge.PrecedenceRequired}
	if got := l.history(); !slices.Equal(got, want) {
		t.Errorf("updates = %v, want %v", got, want)
	}
}

func TestGateDoesNothingWhenConfiguredFailed(t *testing.T) {
	l := &fakeListener{}
	g := newGate(l, edge.PrecedenceFailed, neverBack, discard)
	defer g.stop()
	g.failed()
	g.ok()
	if got := l.history(); len(got) != 0 {
		t.Errorf("updates = %v, want none", got)
	}
}

func TestGateProbeRestores(t *testing.T) {
	l := &fakeListener{}
	var calls atomic.Int32
	probe := func(context.Context) bool { return calls.Add(1) >= 2 }
	g := newGate(l, edge.PrecedenceDefault, probe, discard)
	defer g.stop()

	g.failed()
	deadline := time.Now().Add(5 * time.Second)
	for len(l.history()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Lowered, lowered again after the first probe failed, restored.
	want := []edge.Precedence{edge.PrecedenceFailed, edge.PrecedenceFailed, edge.PrecedenceDefault}
	if got := l.history(); !slices.Equal(got, want) {
		t.Errorf("updates = %v, want %v", got, want)
	}
}

// The SDK binds later terminators with the original precedence, so the
// gate lowers it again after every unsuccessful probe.
func TestGateLowersAgainWhileDown(t *testing.T) {
	l := &fakeListener{}
	g := newGate(l, edge.PrecedenceRequired, neverBack, discard)
	defer g.stop()

	g.failed()
	deadline := time.Now().Add(5 * time.Second)
	for len(l.history()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	for i, p := range l.history() {
		if p != edge.PrecedenceFailed {
			t.Errorf("update %d = %v while down, want failed", i, p)
		}
	}
	if n := len(l.history()); n < 2 {
		t.Errorf("%d updates, want the lowering and at least one more", n)
	}
}

func TestGateStopEndsProbing(t *testing.T) {
	l := &fakeListener{}
	var calls atomic.Int32
	g := newGate(l, edge.PrecedenceDefault, func(context.Context) bool { calls.Add(1); return true }, discard)
	g.failed()
	g.stop()
	time.Sleep(probeMinInterval + 200*time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Errorf("probe ran %d times after stop", n)
	}
	g.ok()
	if got := l.history(); len(got) != 1 {
		t.Errorf("updates = %v, want only the lowering", got)
	}
}
