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
	updates []terms
}

func (f *fakeListener) UpdateCostAndPrecedence(cost uint16, p edge.Precedence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, terms{precedence: p, cost: cost})
	return nil
}

// history is the precedence of every update, in order.
func (f *fakeListener) history() []edge.Precedence {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ps []edge.Precedence
	for _, u := range f.updates {
		ps = append(ps, u.precedence)
	}
	return ps
}

func (f *fakeListener) all() []terms {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]terms(nil), f.updates...)
}

func boundAt(p edge.Precedence) terms { return terms{precedence: p} }

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func neverBack(context.Context) bool { return false }

// fail records a failed dial and waits for the probe that confirms it.
func fail(t *testing.T, g *gate) {
	t.Helper()
	g.failed()
	settle(t, g)
}

// settle waits for the confirming probe to finish.
func settle(t *testing.T, g *gate) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for g.confirming.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the confirming probe did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGateLowersOnceAndRestoresOnSuccess(t *testing.T) {
	l := &fakeListener{}
	g := newGate(l, boundAt(edge.PrecedenceRequired), neverBack, discard)
	defer g.stop()

	g.ok() // nothing to restore yet
	fail(t, g)
	g.failed() // already down
	g.ok()
	g.ok()

	want := []edge.Precedence{edge.PrecedenceFailed, edge.PrecedenceRequired}
	if got := l.history(); !slices.Equal(got, want) {
		t.Errorf("updates = %v, want %v", got, want)
	}
}

// A failed dial whose probe succeeds does not gate: the egress host may
// have rejected that dialer's own app data.
func TestGateConfirmsAFailureBeforeLowering(t *testing.T) {
	l := &fakeListener{}
	g := newGate(l, boundAt(edge.PrecedenceRequired), func(context.Context) bool { return true }, discard)
	defer g.stop()

	fail(t, g)
	if g.isDown() || len(l.history()) != 0 {
		t.Errorf("gated by one failure the probe did not confirm: down %v, updates %v", g.isDown(), l.history())
	}
}

// While the confirming probe runs, more failures start no other probe,
// and a dial that succeeds meanwhile means there is nothing to gate.
func TestGateSuccessWhileConfirming(t *testing.T) {
	l := &fakeListener{}
	var calls atomic.Int32
	release := make(chan struct{})
	probe := func(context.Context) bool {
		calls.Add(1)
		<-release
		return false
	}
	g := newGate(l, boundAt(edge.PrecedenceRequired), probe, discard)
	defer g.stop()

	g.failed()
	g.failed()
	g.failed()
	g.ok()
	close(release)
	settle(t, g)
	if n := calls.Load(); n != 1 {
		t.Errorf("%d probes, want 1", n)
	}
	if g.isDown() || len(l.history()) != 0 {
		t.Errorf("gated despite a dial that succeeded: down %v, updates %v", g.isDown(), l.history())
	}
}

func TestGateDoesNothingWhenConfiguredFailed(t *testing.T) {
	l := &fakeListener{}
	g := newGate(l, boundAt(edge.PrecedenceFailed), neverBack, discard)
	defer g.stop()
	fail(t, g)
	g.ok()
	if got := l.history(); len(got) != 0 {
		t.Errorf("updates = %v, want none", got)
	}
}

func TestGateProbeRestores(t *testing.T) {
	l := &fakeListener{}
	var calls atomic.Int32
	probe := func(context.Context) bool { return calls.Add(1) >= 3 }
	g := newGate(l, boundAt(edge.PrecedenceDefault), probe, discard)
	defer g.stop()

	g.failed()
	deadline := time.Now().Add(5 * time.Second)
	for len(l.history()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Lowered once the confirming probe failed, lowered again after the
	// next one failed, restored.
	want := []edge.Precedence{edge.PrecedenceFailed, edge.PrecedenceFailed, edge.PrecedenceDefault}
	if got := l.history(); !slices.Equal(got, want) {
		t.Errorf("updates = %v, want %v", got, want)
	}
}

// The SDK binds later terminators with the original precedence, so the
// gate lowers it again after every unsuccessful probe.
func TestGateLowersAgainWhileDown(t *testing.T) {
	l := &fakeListener{}
	g := newGate(l, boundAt(edge.PrecedenceRequired), neverBack, discard)
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
	// Fails only the confirming probe.
	g := newGate(l, boundAt(edge.PrecedenceDefault), func(context.Context) bool { return calls.Add(1) > 1 }, discard)
	fail(t, g)
	g.stop()
	time.Sleep(probeMinInterval + 200*time.Millisecond)
	if n := calls.Load() - 1; n != 0 {
		t.Errorf("probe ran %d times after stop", n)
	}
	g.ok()
	if got := l.history(); len(got) != 1 {
		t.Errorf("updates = %v, want only the lowering", got)
	}
}

func TestGateRetune(t *testing.T) {
	l := &fakeListener{}
	g := newGate(l, boundAt(edge.PrecedenceRequired), neverBack, discard)
	defer g.stop()

	g.terminatorsChanged() // nothing differs from what the listener was created with
	if got := l.all(); len(got) != 0 {
		t.Fatalf("updates = %v, want none", got)
	}
	want := terms{precedence: edge.PrecedenceDefault, cost: 5}
	if err := g.retune(want); err != nil {
		t.Fatal(err)
	}
	// A terminator the SDK binds now gets the listener's original values,
	// so the new ones go out again.
	g.terminatorsChanged()
	if got := l.all(); !slices.Equal(got, []terms{want, want}) {
		t.Errorf("updates = %v, want %v twice", got, want)
	}
}

func TestGateRetuneWhileDown(t *testing.T) {
	l := &fakeListener{}
	g := newGate(l, boundAt(edge.PrecedenceDefault), neverBack, discard)
	defer g.stop()

	fail(t, g)
	if err := g.retune(terms{precedence: edge.PrecedenceRequired, cost: 9}); err != nil {
		t.Fatal(err)
	}
	g.ok()
	want := []terms{
		{precedence: edge.PrecedenceFailed},            // lowered
		{precedence: edge.PrecedenceFailed, cost: 9},   // new cost, still gated
		{precedence: edge.PrecedenceRequired, cost: 9}, // restored to the new precedence
	}
	if got := l.all(); !slices.Equal(got, want) {
		t.Errorf("updates = %v, want %v", got, want)
	}
}

func TestGateRetuneToFailedEndsGating(t *testing.T) {
	l := &fakeListener{}
	g := newGate(l, boundAt(edge.PrecedenceDefault), neverBack, discard)
	defer g.stop()

	fail(t, g)
	if err := g.retune(boundAt(edge.PrecedenceFailed)); err != nil {
		t.Fatal(err)
	}
	if g.isDown() {
		t.Error("still gated after being configured failed")
	}
}
