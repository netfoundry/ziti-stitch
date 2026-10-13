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
	"errors"
	"sync/atomic"
	"testing"

	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/sdk-golang/ziti/edge"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

// ingressConn is an accepted ingress dial. Only what handle uses on its
// refusal and early-exit paths is implemented.
type ingressConn struct {
	edge.Conn
	appData   []byte
	refusedBy error
	closed    atomic.Bool
}

func (c *ingressConn) GetAppData() []byte             { return c.appData }
func (c *ingressConn) GetCircuitId() string           { return "circ" }
func (c *ingressConn) GetDialerIdentityId() string    { return "id" }
func (c *ingressConn) GetDialerIdentityName() string  { return "client" }
func (c *ingressConn) CompleteAcceptFailed(err error) { c.refusedBy = err }
func (c *ingressConn) CompleteAcceptSuccess() error   { return errors.New("the dialer went away") }
func (c *ingressConn) Close() error                   { c.closed.Store(true); return nil }

// egressConn is a successful egress dial.
type egressConn struct{ edge.Conn }

func (egressConn) GetCircuitId() string { return "circB" }
func (egressConn) Close() error         { return nil }

// egress is the egress network's context: its dials fail with err.
type egress struct {
	ziti.Context
	err error
}

func (e *egress) DialWithOptions(string, *ziti.DialOptions) (edge.Conn, error) {
	if e.err != nil {
		return nil, e.err
	}
	return egressConn{}, nil
}

type namedNetwork string

func (n namedNetwork) Name() string                                { return string(n) }
func (namedNetwork) Context(context.Context) (ziti.Context, error) { return nil, errors.New("unused") }

func testStitch(appData config.AppDataPolicy) *Stitch {
	return New(config.Stitch{
		Name:    "s",
		From:    config.Ingress{Network: "a", Service: "in", Precedence: config.PrecedenceDefault},
		To:      config.Egress{Network: "b", Service: "out"},
		AppData: appData,
	}, namedNetwork("a"), namedNetwork("b"), discard)
}

func TestRefusedDialsAreClosed(t *testing.T) {
	t.Run("egress unreachable", func(t *testing.T) {
		s := testStitch(config.AppDataPassthrough)
		in := &ingressConn{}
		g := newGate(&fakeListener{}, boundAt(edge.PrecedenceDefault), neverBack, discard)
		defer g.stop()
		s.handle(in, &egress{err: errors.New("no terminators")}, g)
		if in.refusedBy != errUpstreamUnavailable || !in.closed.Load() {
			t.Errorf("refused with %v, closed %v; want %v and closed", in.refusedBy, in.closed.Load(), errUpstreamUnavailable)
		}
	})
	t.Run("loop", func(t *testing.T) {
		s := testStitch(config.AppDataAnnotate)
		in := &ingressConn{appData: []byte(`{"ziti_stitch":{"hops":8}}`)}
		s.handle(in, nil, nil) // refused before the egress side is used
		if in.refusedBy != errLoopRefused || !in.closed.Load() {
			t.Errorf("refused with %v, closed %v; want %v and closed", in.refusedBy, in.closed.Load(), errLoopRefused)
		}
	})
}

// The probe dials with the app data of the last successful egress dial,
// so one client whose app data the egress host rejects cannot keep the
// stitch gated. Before any success it uses the first failure's.
func TestProbeAppData(t *testing.T) {
	s := testStitch(config.AppDataPassthrough)
	g := newGate(&fakeListener{}, boundAt(edge.PrecedenceDefault), neverBack, discard)
	defer g.stop()
	probe := func() string {
		if p := s.probeAppData.Load(); p != nil {
			return string(*p)
		}
		return ""
	}
	dial := func(appData string, err error) {
		s.handle(&ingressConn{appData: []byte(appData)}, &egress{err: err}, g)
	}
	down := errors.New("dial failed")

	dial("first-failure", down)
	dial("second-failure", down)
	if got := probe(); got != "first-failure" {
		t.Errorf("before any success, probe app data = %q, want first-failure", got)
	}
	dial("good", nil)
	dial("rejected-by-host", down)
	if got := probe(); got != "good" {
		t.Errorf("probe app data = %q, want good", got)
	}
}
