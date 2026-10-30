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
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openziti/sdk-golang/ziti/edge"

	"github.com/netfoundry/ziti-stitch/internal/retry"
)

const (
	probeMinInterval = time.Second
	probeMaxInterval = 30 * time.Second
)

// tuner is the part of edge.Listener the gate needs.
type tuner interface {
	UpdateCostAndPrecedence(cost uint16, precedence edge.Precedence) error
}

// terms is how a listener's terminators compete for dials.
type terms struct {
	precedence edge.Precedence
	cost       uint16
}

// gate drops a listener's terminators to precedence failed while the
// egress service is unreachable, so the ingress controller sends dials to
// instances that can complete them, and restores the configured precedence
// once the egress service is back. It also applies cost and precedence
// changes to the live listener, so changing them needs no rebind.
//
// Without it, an instance configured as required keeps winning every dial
// while it cannot reach the egress side, and the controller's retries never
// reach a standby. Precedence is ours to change on our own terminators, so
// this needs no admin rights.
//
// One failed dial does not gate: it may be the dialer's own doing (app
// data the egress host rejects, say), and any dialer could otherwise take
// instances out of rotation at will. The gate first runs probe once, and
// lowers precedence only if that fails too and no dial has succeeded
// meanwhile.
//
// A gated instance learns the egress side is back in one of two ways: a
// dial it still receives (it is still the last resort) succeeds, or probe
// reports true. Probe runs on a backoff while gated.
//
// The SDK binds terminators it adds later (a router reconnecting, a new
// router) with the cost and precedence the listener was created with
// (bound), and an update only reaches the terminators that exist when it
// is sent. So whenever terminators come and go (terminatorsChanged), and
// after every unsuccessful probe, the gate applies what it wants again if
// that differs from bound.
type gate struct {
	l     tuner
	bound terms
	probe func(context.Context) bool
	log   *slog.Logger

	ctx    context.Context // done once the gate stops
	cancel context.CancelFunc

	// down is read without mu on every connection; mu serializes the
	// changes, which talk to the router and can take seconds.
	down        atomic.Bool
	confirming  atomic.Bool   // a failed dial's probe is running
	oks         atomic.Uint64 // successful dials, so confirming can tell one happened
	mu          sync.Mutex
	want        terms // the configured cost and precedence
	stopped     bool
	cancelProbe context.CancelFunc
}

func newGate(l tuner, bound terms, probe func(context.Context) bool, log *slog.Logger) *gate {
	ctx, cancel := context.WithCancel(context.Background())
	return &gate{l: l, bound: bound, want: bound, probe: probe, log: log, ctx: ctx, cancel: cancel}
}

// apply sends the cost and the precedence the terminators should have
// now. The caller holds mu.
func (g *gate) apply() error {
	p := g.want.precedence
	if g.down.Load() {
		p = edge.PrecedenceFailed
	}
	return g.l.UpdateCostAndPrecedence(g.want.cost, p)
}

// failed records a failed egress dial, and starts the probe that decides
// whether to gate unless one is running already.
func (g *gate) failed() {
	if g.down.Load() || !g.confirming.CompareAndSwap(false, true) {
		return
	}
	go g.confirmDown(g.oks.Load())
}

// confirmDown gates if the probe fails too and no dial has succeeded since
// the failure, when oks had that value.
func (g *gate) confirmDown(oks uint64) {
	defer g.confirming.Store(false)
	if g.probe(g.ctx) {
		return
	}
	g.lower(oks)
}

// lower drops the precedence to failed and probes until the egress
// service is back, unless a dial has succeeded since oks was read.
func (g *gate) lower(oks uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.down.Load() || g.stopped || g.want.precedence == edge.PrecedenceFailed {
		return
	}
	// down before the count: ok counts before it looks at down, so a
	// success either shows in the count here or finds down set and
	// restores the precedence itself.
	g.down.Store(true)
	if g.oks.Load() != oks {
		g.down.Store(false)
		return
	}
	if err := g.apply(); err != nil {
		g.log.Error("egress service unreachable, and lowering precedence failed", "error", err)
	} else {
		g.log.Warn("egress service unreachable; lowered precedence to failed so other instances get the dials")
	}
	ctx, cancel := context.WithCancel(g.ctx)
	g.cancelProbe = cancel
	go g.probeUntilBack(ctx)
}

// ok records a successful egress dial.
func (g *gate) ok() {
	g.oks.Add(1)
	if !g.down.Load() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.down.Load() || g.stopped {
		return
	}
	g.down.Store(false)
	g.cancelProbe()
	if err := g.apply(); err != nil {
		g.log.Error("egress service is back, but restoring precedence failed", "error", err)
		return
	}
	g.log.Info("egress service is back; restored precedence")
}

// retune applies a new configured cost and precedence. While gated, the
// precedence stays failed and the new one is what ok restores.
func (g *gate) retune(t terms) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return nil
	}
	g.want = t
	if t.precedence == edge.PrecedenceFailed && g.down.Load() {
		g.down.Store(false) // nothing left to gate
		g.cancelProbe()
	}
	return g.apply()
}

// terminatorsChanged re-applies what the gate wants when the SDK may have
// bound terminators with bound's cost and precedence instead.
func (g *gate) terminatorsChanged() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped || (!g.down.Load() && g.want == g.bound) {
		return
	}
	if err := g.apply(); err != nil {
		g.log.Debug("re-applying cost and precedence failed", "error", err)
	}
}

// isDown reports whether the gate has lowered the listener's precedence.
func (g *gate) isDown() bool {
	return g.down.Load()
}

// stop ends probing. The listener is going away, so its terminators no
// longer matter.
func (g *gate) stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
	g.cancel()
}

func (g *gate) probeUntilBack(ctx context.Context) {
	b := retry.Backoff{Min: probeMinInterval, Max: probeMaxInterval}
	for retry.Sleep(ctx, b.Next()) {
		if g.probe(ctx) {
			g.ok()
			return
		}
		g.terminatorsChanged()
	}
}
