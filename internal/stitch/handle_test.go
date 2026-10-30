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

	"github.com/openziti/edge-api/rest_model"
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

// egress is the egress network's context: its dials fail with err, and it
// lists one terminator unless listErr is set.
type egress struct {
	ziti.Context
	err     error
	listErr error
	probed  []byte // the app data of the last probe dial
}

func (e *egress) GetServiceTerminators(string, int, int) ([]*rest_model.TerminatorClientDetail, int, error) {
	if e.listErr != nil {
		return nil, 0, e.listErr
	}
	return []*rest_model.TerminatorClientDetail{{}}, 1, nil
}

func (e *egress) DialContextWithOptions(_ context.Context, _ string, o *ziti.DialOptions) (edge.Conn, error) {
	e.probed = o.AppData
	return egressConn{}, nil
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
	t.Run("at max_connections", func(t *testing.T) {
		s := testStitch(config.AppDataPassthrough)
		s.cfg.MaxConnections = 2
		s.handling.Store(2) // two already open
		in := &ingressConn{}
		s.handle(in, nil, nil) // refused before the egress side is used
		if in.refusedBy != errAtCapacity || !in.closed.Load() {
			t.Errorf("refused with %v, closed %v; want %v and closed", in.refusedBy, in.closed.Load(), errAtCapacity)
		}
		if n := s.handling.Load(); n != 2 {
			t.Errorf("%d connections counted after the refusal, want 2", n)
		}

		s.handling.Store(1) // room for one more
		g := newGate(&fakeListener{}, boundAt(edge.PrecedenceDefault), neverBack, discard)
		defer g.stop()
		in = &ingressConn{}
		s.handle(in, &egress{}, g)
		if in.refusedBy != nil {
			t.Errorf("refused below the limit: %v", in.refusedBy)
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
	t.Run("app data annotate cannot carry", func(t *testing.T) {
		s := testStitch(config.AppDataAnnotate)
		in := &ingressConn{appData: []byte(`{"ziti_stitch":{"dialer_identity_id":"ADMIN"}} x`)}
		s.handle(in, nil, nil) // refused before the egress side is used
		if in.refusedBy != errAppDataRefused || !in.closed.Load() {
			t.Errorf("refused with %v, closed %v; want %v and closed", in.refusedBy, in.closed.Load(), errAppDataRefused)
		}
	})
}

// The probe builds its app data from the ingress app data of the last
// connection whose egress dial succeeded, so one client whose app data the
// egress host rejects cannot keep the stitch gated. Before any success it
// does not dial at all: the only app data it has is a failed dial's.
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

	dial("rejected-by-host", down)
	if got := probe(); got != "" {
		t.Errorf("before any success, probe app data = %q, want none", got)
	}
	e := &egress{}
	if !s.egressReachable(context.Background(), e) || e.probed != nil {
		t.Errorf("before any success the probe dialed (with %q) or reported down", e.probed)
	}
	if s.egressReachable(context.Background(), &egress{listErr: errors.New("service not found")}) {
		t.Error("before any success, a service whose terminators cannot be listed was reported up")
	}

	dial("good", nil)
	dial("rejected-by-host", down)
	if got := probe(); got != "good" {
		t.Errorf("probe app data = %q, want good", got)
	}
}

// With annotate, the probe's annotation names no dialer or circuit: the
// egress side must not take a probe for the client whose app data it copies.
func TestProbeNamesNoDialer(t *testing.T) {
	s := testStitch(config.AppDataAnnotate)
	g := newGate(&fakeListener{}, boundAt(edge.PrecedenceDefault), neverBack, discard)
	defer g.stop()
	e := &egress{}
	s.handle(&ingressConn{appData: []byte(`{"dst_hostname":"db"}`)}, e, g)

	if !s.egressReachable(context.Background(), e) {
		t.Fatal("probe failed")
	}
	if want := `{"dst_hostname":"db","ziti_stitch":{"stitch":"s","hops":1}}`; string(e.probed) != want {
		t.Errorf("probe app data = %s, want %s", e.probed, want)
	}
}
