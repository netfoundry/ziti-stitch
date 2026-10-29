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
	"github.com/netfoundry/ziti-stitch/internal/network"
	"github.com/netfoundry/ziti-stitch/internal/stitch"
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

func (f *fakeNetwork) Status() network.Status { return network.Status{Name: f.name} }

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
	f.rec.add("stitch start %s v=%d", f.cfg.Name, version(f.cfg))
	<-ctx.Done()
	f.rec.add("stitch stop %s v=%d", f.cfg.Name, version(f.cfg))
}

func (f *fakeStitch) Retune(in config.Ingress) {
	f.rec.add("stitch retune %s v=%d cost=%d precedence=%s", f.cfg.Name, version(f.cfg), in.Cost, in.Precedence)
}

// version tells instances of a stitch apart in the tests: a setting that
// changing restarts the stitch.
func version(s config.Stitch) int { return int(s.IdleTimeout / time.Minute) }

func (f *fakeStitch) Wait() { <-f.drained }

func (f *fakeStitch) Status() stitch.Status { return stitch.Status{Name: f.cfg.Name} }

func (f *fakeStitch) Connections() []stitch.ConnStatus {
	return []stitch.ConnStatus{{Stitch: f.cfg.Name, ACircuit: fmt.Sprintf("%s-v%d", f.cfg.Name, version(f.cfg))}}
}

type harness struct {
	r   *Runner
	rec *recorder

	mu          sync.Mutex
	stitches    map[string][]*fakeStitch // instances by "name v", oldest first
	commonNames map[string]string        // by identity file; see identity
}

func newHarness() *harness {
	h := &harness{rec: &recorder{}, stitches: map[string][]*fakeStitch{}}
	h.r = newRunner(slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(name, path string) Network { return &fakeNetwork{name: name, path: path, rec: h.rec} },
		func(cfg config.Stitch, _, _ Network) Stitch {
			f := &fakeStitch{cfg: cfg, rec: h.rec, drained: make(chan struct{})}
			h.mu.Lock()
			key := fmt.Sprintf("%s v=%d", cfg.Name, version(cfg))
			h.stitches[key] = append(h.stitches[key], f)
			h.mu.Unlock()
			return f
		},
		func(path string) string {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.commonNames[path]
		})
	return h
}

// enroll puts a different identity in the file at path.
func (h *harness) enroll(path, commonName string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.commonNames == nil {
		h.commonNames = map[string]string{}
	}
	h.commonNames[path] = commonName
}

// drain lets the connections of the oldest instance with that name and
// version finish.
func (h *harness) drain(name string, v int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := fmt.Sprintf("%s v=%d", name, v)
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

// st is a stitch at version v (see version).
func st(name, from, to string, v int) config.Stitch {
	return config.Stitch{
		Name:        name,
		From:        config.Ingress{Network: from, Service: "svc"},
		To:          config.Egress{Network: to, Service: "svc"},
		IdleTimeout: time.Duration(v) * time.Minute,
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
	h.rec.waitFor(t, "stitch start s1 v=0")
	h.rec.waitFor(t, "net start a@a.json")
	h.rec.waitFor(t, "net start b@b.json")
	h.rec.settle()
	if h.rec.has("net start c@c.json") || h.rec.has("stitch start off v=0") {
		t.Errorf("started something unused or disabled: %v", h.rec.events)
	}
}

func TestApplySameConfigChangesNothing(t *testing.T) {
	h := newHarness()
	d := dir(abNets, st("s1", "a", "b", 0))
	h.r.Apply(d)
	h.rec.waitFor(t, "stitch start s1 v=0")
	if c := h.r.Apply(d); c != (Changes{}) {
		t.Errorf("changes = %+v", c)
	}
	h.rec.settle()
	if n := h.rec.count("stitch start s1 v=0"); n != 1 || h.rec.has("stitch stop s1 v=0") {
		t.Errorf("re-applying the same config touched the stitch: %v", h.rec.events)
	}
}

func TestApplyRestartsChangedStitchWithoutRestartingNetworks(t *testing.T) {
	h := newHarness()
	h.r.Apply(dir(abNets, st("s1", "a", "b", 0)))
	h.rec.waitFor(t, "stitch start s1 v=0")

	c := h.r.Apply(dir(abNets, st("s1", "a", "b", 7)))
	if c != (Changes{Restarted: 1}) {
		t.Errorf("changes = %+v", c)
	}
	h.rec.waitFor(t, "stitch stop s1 v=0")
	h.rec.waitFor(t, "stitch start s1 v=7")
	h.drain("s1", 0)
	h.rec.settle()
	if h.rec.has("net stop a@a.json") || h.rec.count("net start a@a.json") != 1 {
		t.Errorf("networks were restarted: %v", h.rec.events)
	}
}

func TestRemovedNetworkStopsOnlyAfterItsConnectionsDrain(t *testing.T) {
	h := newHarness()
	h.r.Apply(dir(map[string]string{"a": "a.json", "b": "b.json", "c": "c.json"}, st("s1", "a", "b", 0), st("s2", "a", "c", 0)))
	h.rec.waitFor(t, "stitch start s2 v=0")

	c := h.r.Apply(dir(map[string]string{"a": "a.json", "b": "b.json"}, st("s1", "a", "b", 0)))
	if c != (Changes{Stopped: 1}) {
		t.Errorf("changes = %+v", c)
	}
	h.rec.waitFor(t, "stitch stop s2 v=0")
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
	h.rec.waitFor(t, "stitch start s1 v=0")

	c := h.r.Apply(dir(map[string]string{"a": "a2.json", "b": "b.json"}, st("s1", "a", "b", 0)))
	if c != (Changes{Restarted: 1}) {
		t.Errorf("changes = %+v", c)
	}
	h.rec.waitFor(t, "net start a@a2.json")
	h.rec.waitFor(t, "stitch stop s1 v=0")
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
	h.rec.waitFor(t, "stitch start s1 v=0")

	done := make(chan bool)
	go func() { done <- h.r.Shutdown(time.Minute) }()
	h.rec.waitFor(t, "stitch stop s1 v=0")
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
	h.rec.waitFor(t, "stitch start s1 v=0")
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

// stuckNetwork does not stop when asked, like one in the middle of a
// certificate renewal, until released.
type stuckNetwork struct {
	fakeNetwork
	release chan struct{}
}

func (f *stuckNetwork) Run(context.Context) { <-f.release }

func TestShutdownDoesNotWaitForeverForNetworks(t *testing.T) {
	h := newHarness()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.r.newNetwork = func(name, path string) Network {
		return &stuckNetwork{fakeNetwork{name: name, path: path, rec: h.rec}, release}
	}
	h.r.netStopWait = 50 * time.Millisecond
	h.r.Apply(dir(abNets, st("s1", "a", "b", 0)))
	h.rec.waitFor(t, "stitch start s1 v=0")
	h.drain("s1", 0)

	done := make(chan bool)
	go func() { done <- h.r.Shutdown(time.Second) }()
	select {
	case inTime := <-done:
		if !inTime {
			t.Error("the connections drained, but Shutdown reported they did not")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown waited for networks that never stop")
	}
}

func TestSnapshotIncludesDrainingConnections(t *testing.T) {
	h := newHarness()
	h.r.Apply(dir(abNets, st("s1", "a", "b", 0), st("s2", "a", "b", 0)))
	h.rec.waitFor(t, "stitch start s2 v=0")
	h.r.Apply(dir(abNets, st("s1", "a", "b", 0), st("s2", "a", "b", 5)))
	h.rec.waitFor(t, "stitch start s2 v=5")

	snap := h.r.Snapshot()
	var names []string
	for _, n := range snap.Networks {
		names = append(names, n.Name)
	}
	if !slices.Equal(names, []string{"a", "b"}) {
		t.Errorf("networks = %v", names)
	}
	if len(snap.Stitches) != 2 || snap.Stitches[0].Name != "s1" || snap.Stitches[1].Name != "s2" {
		t.Errorf("stitches = %+v", snap.Stitches)
	}
	if snap.Draining != 1 {
		t.Errorf("draining = %d, want the old s2", snap.Draining)
	}
	var circuits []string
	for _, c := range snap.Connections {
		circuits = append(circuits, c.ACircuit)
	}
	slices.Sort(circuits)
	if !slices.Equal(circuits, []string{"s1-v0", "s2-v0", "s2-v5"}) {
		t.Errorf("connections = %v", circuits)
	}

	h.drain("s2", 0)
	deadline := time.Now().Add(2 * time.Second)
	for h.r.Snapshot().Draining != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if d := h.r.Snapshot().Draining; d != 0 {
		t.Errorf("draining = %d after it drained", d)
	}
}

func TestWebNetworkRunsWithoutStitches(t *testing.T) {
	h := newHarness()
	d := dir(map[string]string{"a": "a.json", "b": "b.json", "w": "w.json"}, st("s1", "a", "b", 0))
	d.Config.Web.Ziti = &config.WebZiti{Network: "w", Service: "admin", Hosts: []string{"h"}}
	h.r.Apply(d)
	h.rec.waitFor(t, "net start w@w.json")
	if n, ok := h.r.Network("w"); !ok || n.Name() != "w" {
		t.Fatalf("Network(w) = %v, %v", n, ok)
	}

	// Pinning a network a stitch also uses must not start it twice.
	d.Config.Web.Ziti.Network = "a"
	h.r.Apply(d)
	h.rec.waitFor(t, "net stop w@w.json")
	h.rec.settle()
	if n := h.rec.count("net start a@a.json"); n != 1 {
		t.Errorf("network a started %d times", n)
	}

	d.Config.Web.Ziti = nil
	h.r.Apply(d)
	h.rec.settle()
	if h.rec.has("net stop a@a.json") {
		t.Error("network a stopped although s1 still uses it")
	}
	if _, ok := h.r.Network("w"); ok {
		t.Error("Network(w) after it stopped")
	}
}

func TestApplyRetunesPrecedenceAndCostInPlace(t *testing.T) {
	h := newHarness()
	h.r.Apply(dir(abNets, st("s1", "a", "b", 0)))
	h.rec.waitFor(t, "stitch start s1 v=0")

	changed := st("s1", "a", "b", 0)
	changed.From.Cost, changed.From.Precedence = 7, config.PrecedenceRequired
	if c := h.r.Apply(dir(abNets, changed)); c != (Changes{Retuned: 1}) {
		t.Errorf("changes = %+v", c)
	}
	h.rec.waitFor(t, "stitch retune s1 v=0 cost=7 precedence=required")
	if c := h.r.Apply(dir(abNets, changed)); c != (Changes{}) {
		t.Errorf("re-applying after a retune: changes = %+v", c)
	}
	h.rec.settle()
	if h.rec.has("stitch stop s1 v=0") {
		t.Errorf("a precedence and cost change restarted the stitch: %v", h.rec.events)
	}
}

func TestSameExceptTerms(t *testing.T) {
	a := st("s1", "a", "b", 0)
	b := a
	b.From.Cost, b.From.Precedence = 3, config.PrecedenceFailed
	if !sameExceptTerms(a, b) {
		t.Error("cost and precedence alone should not count as a change")
	}
	b.From.Service = "other"
	if sameExceptTerms(a, b) {
		t.Error("a different ingress service should count as a change")
	}
}

// A network removed and enrolled again under the same name has a new
// identity in the same file. It needs a new context even while a stitch on
// the old one drains, and so do the stitches on it.
func TestReenrolledNetworkGetsANewContext(t *testing.T) {
	h := newHarness()
	h.enroll("a.json", "id-1")
	h.r.Apply(dir(abNets, st("s1", "a", "b", 0), st("s2", "a", "b", 0)))
	h.rec.waitFor(t, "stitch start s1 v=0")
	h.rec.waitFor(t, "stitch start s2 v=0")

	// s1 removed, a enrolled again, all in one reload; s1 keeps draining.
	h.enroll("a.json", "id-2")
	c := h.r.Apply(dir(abNets, st("s2", "a", "b", 0)))
	if c != (Changes{Restarted: 1, Stopped: 1}) {
		t.Errorf("changes = %+v, want s2 restarted and s1 stopped", c)
	}
	h.rec.waitFor(t, "stitch stop s2 v=0")
	h.rec.settle()
	if n := h.rec.count("net start a@a.json"); n != 2 {
		t.Fatalf("network a started %d times, want a second context for the new identity: %v", n, h.rec.events)
	}
	if n := h.rec.count("stitch start s2 v=0"); n != 2 {
		t.Errorf("s2 started %d times, want it restarted on the new identity", n)
	}
	if h.rec.has("net stop a@a.json") {
		t.Errorf("the old context stopped while s1 drains: %v", h.rec.events)
	}

	h.drain("s1", 0)
	h.drain("s2", 0)
	h.rec.waitFor(t, "net stop a@a.json")
	h.rec.settle()
	if n := h.rec.count("net stop a@a.json"); n != 1 {
		t.Errorf("network a stopped %d times, want only the old context", n)
	}

	// A renewal keeps the common name, so nothing restarts.
	if c := h.r.Apply(dir(abNets, st("s2", "a", "b", 0))); c != (Changes{}) {
		t.Errorf("same identity, changes = %+v", c)
	}
}
