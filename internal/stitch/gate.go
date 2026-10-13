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
	"time"

	"github.com/openziti/sdk-golang/ziti/edge"

	"github.com/netfoundry/ziti-stitch/internal/retry"
)

const (
	probeMinInterval = time.Second
	probeMaxInterval = 30 * time.Second
)

// precedenceSetter is the part of edge.Listener the gate needs.
type precedenceSetter interface {
	UpdatePrecedence(edge.Precedence) error
}

// gate drops a listener's terminators to precedence failed while the
// egress service is unreachable, so the ingress controller sends dials to
// instances that can complete them, and restores the configured precedence
// once the egress service is back.
//
// Without it, an instance configured as required keeps winning every dial
// while it cannot reach the egress side, and the controller's retries never
// reach a standby. Precedence is ours to change on our own terminators, so
// this needs no admin rights.
//
// A gated instance learns the egress side is back in one of two ways: a
// dial it still receives (it is still the last resort) succeeds, or probe
// reports true. Probe runs on a backoff while gated.
type gate struct {
	l      precedenceSetter
	normal edge.Precedence
	probe  func(context.Context) bool
	log    *slog.Logger

	mu          sync.Mutex
	down        bool
	stopped     bool
	cancelProbe context.CancelFunc
}

func newGate(l precedenceSetter, normal edge.Precedence, probe func(context.Context) bool, log *slog.Logger) *gate {
	return &gate{l: l, normal: normal, probe: probe, log: log}
}

// failed records a failed egress dial.
func (g *gate) failed() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.down || g.stopped || g.normal == edge.PrecedenceFailed {
		return
	}
	g.down = true
	if err := g.l.UpdatePrecedence(edge.PrecedenceFailed); err != nil {
		g.log.Error("egress service unreachable, and lowering precedence failed", "error", err)
	} else {
		g.log.Warn("egress service unreachable; lowered precedence to failed so other instances get the dials")
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.cancelProbe = cancel
	go g.probeUntilBack(ctx)
}

// ok records a successful egress dial.
func (g *gate) ok() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.down || g.stopped {
		return
	}
	g.down = false
	g.cancelProbe()
	if err := g.l.UpdatePrecedence(g.normal); err != nil {
		g.log.Error("egress service is back, but restoring precedence failed", "error", err)
		return
	}
	g.log.Info("egress service is back; restored precedence")
}

// stop ends probing. The listener is going away, so its precedence no
// longer matters.
func (g *gate) stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
	if g.cancelProbe != nil {
		g.cancelProbe()
	}
}

func (g *gate) probeUntilBack(ctx context.Context) {
	b := retry.Backoff{Min: probeMinInterval, Max: probeMaxInterval}
	for retry.Sleep(ctx, b.Next()) {
		if g.probe(ctx) {
			g.ok()
			return
		}
	}
}
