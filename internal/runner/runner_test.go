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

package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/openziti/sdk-golang/ziti"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

// recorder collects what the fakes did, in order.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf(format, args...))
}

func (r *recorder) has(ev string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.events, ev)
}

func (r *recorder) count(ev string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e == ev {
			n++
		}
	}
	return n
}

func (r *recorder) waitFor(t *testing.T, ev string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !r.has(ev) {
		if time.Now().After(deadline) {
			t.Fatalf("never saw %q; saw %v", ev, r.events)
		}
		time.Sleep(time.Millisecond)
	}
}

// settle gives goroutines a moment to act on something they should not.
func (r *recorder) settle() { time.Sleep(20 * time.Millisecond) }

type fakeNetwork struct {
	name, path string
	rec        *recorder
}

func (f *fakeNetwork) Name() string { return f.name }

func (f *fakeNetwork) Context(context.Context) (ziti.Context, error) { return nil, nil }

func (f *fakeNetwork) Run(ctx context.Context) {
	f.rec.add("net start %s@%s", f.name, f.path)
	<-ctx.Done()
	f.rec.add("net stop %s@%s", f.name, f.path)
}

// fakeStitch's connections finish when its drained channel is closed.
type fakeStitch struct {
	cfg     config.Stitch
	rec     *recorder
	drained chan struct{}
}

func (f *fakeStitch) Run(ctx context.Context) {
	f.rec.add("stitch start %s cost=%d", f.cfg.Name, f.cfg.From.Cost)
	<-ctx.Done()
	f.rec.add("stitch stop %s cost=%d", f.cfg.Name, f.cfg.From.Cost)
}

func (f *fakeStitch) Wait() { <-f.drained }

type harness struct {
	r   *Runner
	rec *recorder

	mu       sync.Mutex
	stitches map[string][]*fakeStitch // instances by "name cost", oldest first
}

func newHarness() *harness {
	h := &harness{rec: &recorder{}, stitches: map[string][]*fakeStitch{}}
	h.r = newRunner(slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(name, path string) Network { return &fakeNetwork{name: name, path: path, rec: h.rec} },
		func(cfg config.Stitch, _, _ Network) Stitch {
			f := &fakeStitch{cfg: cfg, rec: h.rec, drained: make(chan struct{})}
			h.mu.Lock()
			key := fmt.Sprintf("%s cost=%d", cfg.Name, cfg.From.Cost)
			h.stitches[key] = append(h.stitches[key], f)
			h.mu.Unlock()
			return f
		})
	return h
}

// drain lets the connections of the oldest instance with that name and
// cost finish.
func (h *harness) drain(name string, cost uint16) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := fmt.Sprintf("%s cost=%d", name, cost)
	close(h.stitches[key][0].drained)
	h.stitches[key] = h.stitches[key][1:]
}

func dir(networks map[string]string, stitches ...config.Stitch) *config.Dir {
	d := &config.Dir{Stitches: stitches}
	for name, path := range networks {
		d.Networks = append(d.Networks, config.Network{Name: name, IdentityFile: path})
	}
	return d
}

func st(name, from, to string, cost uint16) config.Stitch {
	return config.Stitch{
		Name: name,
		From: config.Ingress{Network: from, Service: "svc", Cost: cost},
		To:   config.Egress{Network: to, Service: "svc"},
	}
}

var abNets = map[string]string{"a": "a.json", "b": "b.json"}

func TestApplyStartsOnlyWhatIsEnabled(t *testing.T) {
	h := newHarness()
	off := st("off", "a", "b", 0)
	off.Disabled = true
	c := h.r.Apply(dir(map[string]string{"a": "a.json", "b": "b.json", "c": "c.json"}, st("s1", "a", "b", 0), off))
	if c != (Changes{Started: 1}) {
		t.Errorf("changes = %+v", c)
	}
	h.rec.waitFor(t, "stitch start s1 cost=0")
	h.rec.waitFor(t, "net start a@a.json")
	h.rec.waitFor(t, "net start b@b.json")
	h.rec.settle()
	if h.rec.has("net start c@c.json") || h.rec.has("stitch start off cost=0") {
		t.Errorf("started something unused or disabled: %v", h.rec.events)
	}
}

func TestApplySameConfigChangesNothing(t *testing.T) {
	h := newHarness()
	d := dir(abNets, st("s1", "a", "b", 0))
	h.r.Apply(d)
	h.rec.waitFor(t, "stitch start s1 cost=0")
	if c := h.r.Apply(d); c != (Changes{}) {
		t.Errorf("changes = %+v", c)
	}
	h.rec.settle()
	if n := h.rec.count("stitch start s1 cost=0"); n != 1 || h.rec.has("stitch stop s1 cost=0") {
		t.Errorf("re-applying the same config touched the stitch: %v", h.rec.events)
	}
}

func TestApplyRestartsChangedStitchWithoutRestartingNetworks(t *testing.T) {
	h := newHarness()
	h.r.Apply(dir(abNets, st("s1", "a", "b", 0)))
	h.rec.waitFor(t, "stitch start s1 cost=0")

	c := h.r.Apply(dir(abNets, st("s1", "a", "b", 7)))
	if c != (Changes{Restarted: 1}) {
		t.Errorf("changes = %+v", c)
	}
	h.rec.waitFor(t, "stitch stop s1 cost=0")
	h.rec.waitFor(t, "stitch start s1 cost=7")
	h.drain("s1", 0)
	h.rec.settle()
	if h.rec.has("net stop a@a.json") || h.rec.count("net start a@a.json") != 1 {
		t.Errorf("networks were restarted: %v", h.rec.events)
	}
}

func TestRemovedNetworkStopsOnlyAfterItsConnectionsDrain(t *testing.T) {
	h := newHarness()
	h.r.Apply(dir(map[string]string{"a": "a.json", "b": "b.json", "c": "c.json"}, st("s1", "a", "b", 0), st("s2", "a", "c", 0)))
	h.rec.waitFor(t, "stitch start s2 cost=0")

	c := h.r.Apply(dir(map[string]string{"a": "a.json", "b": "b.json"}, st("s1", "a", "b", 0)))
	if c != (Changes{Stopped: 1}) {
		t.Errorf("changes = %+v", c)
	}
	h.rec.waitFor(t, "stitch stop s2 cost=0")
	h.rec.settle()
	if h.rec.has("net stop c@c.json") {
		t.Fatal("network c stopped while s2 still had connections")
	}
	h.drain("s2", 0)
	h.rec.waitFor(t, "net stop c@c.json")
	h.rec.settle()
	if h.rec.has("net stop a@a.json") {
		t.Error("network a stopped although s1 still uses it")
	}
}

func TestMovedIdentityReplacesNetworkAndRestartsItsStitches(t *testing.T) {
	h := newHarness()
	h.r.Apply(dir(abNets, st("s1", "a", "b", 0)))
	h.rec.waitFor(t, "stitch start s1 cost=0")

	c := h.r.Apply(dir(map[string]string{"a": "a2.json", "b": "b.json"}, st("s1", "a", "b", 0)))
	if c != (Changes{Restarted: 1}) {
		t.Errorf("changes = %+v", c)
	}
	h.rec.waitFor(t, "net start a@a2.json")
	h.rec.waitFor(t, "stitch stop s1 cost=0")
	h.rec.settle()
	if h.rec.has("net stop a@a.json") {
		t.Fatal("old network stopped while the old stitch was still draining")
	}
	h.drain("s1", 0)
	h.rec.waitFor(t, "net stop a@a.json")
	h.rec.settle()
	if h.rec.has("net stop b@b.json") {
		t.Error("network b was stopped although its identity did not move")
	}
}

func TestShutdownDrainsThenStopsNetworks(t *testing.T) {
	h := newHarness()
	h.r.Apply(dir(abNets, st("s1", "a", "b", 0)))
	h.rec.waitFor(t, "stitch start s1 cost=0")

	done := make(chan bool)
	go func() { done <- h.r.Shutdown(time.Minute) }()
	h.rec.waitFor(t, "stitch stop s1 cost=0")
	h.rec.settle()
	if h.rec.has("net stop a@a.json") {
		t.Fatal("network stopped before the stitch drained")
	}
	h.drain("s1", 0)
	if !<-done {
		t.Error("Shutdown reported a timeout")
	}
	if !h.rec.has("net stop a@a.json") || !h.rec.has("net stop b@b.json") {
		t.Errorf("networks still running: %v", h.rec.events)
	}
}

func TestShutdownTimeoutStopsNetworksAnyway(t *testing.T) {
	h := newHarness()
	h.r.Apply(dir(abNets, st("s1", "a", "b", 0)))
	h.rec.waitFor(t, "stitch start s1 cost=0")
	// Connections that never finish on their own; closing the networks is
	// what ends them for real, which the fake does not model.
	defer h.drain("s1", 0)

	if h.r.Shutdown(50 * time.Millisecond) {
		t.Error("Shutdown reported draining in time")
	}
	if !h.rec.has("net stop a@a.json") || !h.rec.has("net stop b@b.json") {
		t.Errorf("networks still running: %v", h.rec.events)
	}
}
