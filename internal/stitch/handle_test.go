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
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

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

// wantRefused fails t unless the dial was refused with want and the
// connection closed, which happens in the background.
func (c *ingressConn) wantRefused(t *testing.T, want error) {
	t.Helper()
	if c.refusedBy != want {
		t.Errorf("refused with %v, want %v", c.refusedBy, want)
	}
	for deadline := time.Now().Add(time.Second); !c.closed.Load(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Error("the refused connection was not closed")
			return
		}
	}
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

// tcpEdge is a Ziti connection played by one end of a loopback TCP
// connection, for handle's success path. It completes accepts, and a read
// after the far end closes reports EOF, as an edge connection's does.
type tcpEdge struct {
	edge.Conn
	tcp     *net.TCPConn
	circuit string
	appData []byte
}

func (c *tcpEdge) Read(b []byte) (int, error)     { return c.tcp.Read(b) }
func (c *tcpEdge) Write(b []byte) (int, error)    { return c.tcp.Write(b) }
func (c *tcpEdge) CloseWrite() error              { return c.tcp.CloseWrite() }
func (c *tcpEdge) Close() error                   { return c.tcp.Close() }
func (c *tcpEdge) IsClosed() bool                 { return false }
func (c *tcpEdge) GetAppData() []byte             { return c.appData }
func (c *tcpEdge) GetCircuitId() string           { return c.circuit }
func (c *tcpEdge) GetDialerIdentityId() string    { return "id" }
func (c *tcpEdge) GetDialerIdentityName() string  { return "client" }
func (c *tcpEdge) CompleteAcceptSuccess() error   { return nil }
func (c *tcpEdge) CompleteAcceptFailed(err error) {}

// egress is the egress network's context: its dials fail with err, or
// return conn when it is set, and it lists one terminator unless listErr
// is set.
type egress struct {
	ziti.Context
	err     error
	listErr error
	conn    edge.Conn
	probed  []byte // the app data of the last dial
}

func (e *egress) GetServiceTerminators(string, int, int) ([]*rest_model.TerminatorClientDetail, int, error) {
	if e.listErr != nil {
		return nil, 0, e.listErr
	}
	return []*rest_model.TerminatorClientDetail{{}}, 1, nil
}

func (e *egress) DialContextWithOptions(ctx context.Context, _ string, o *ziti.DialOptions) (edge.Conn, error) {
	e.probed = o.AppData
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.err != nil {
		return nil, e.err
	}
	if e.conn != nil {
		return e.conn, nil
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
		in.wantRefused(t, errUpstreamUnavailable)
	})
	t.Run("at max_connections", func(t *testing.T) {
		s := testStitch(config.AppDataPassthrough)
		s.cfg.MaxConnections = 2
		s.handling.Store(2) // two already open
		in := &ingressConn{}
		s.handle(in, nil, nil) // refused before the egress side is used
		in.wantRefused(t, errAtCapacity)
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
	t.Run("stitch stopped while dialing", func(t *testing.T) {
		s := testStitch(config.AppDataPassthrough)
		g := newGate(&fakeListener{}, boundAt(edge.PrecedenceDefault), neverBack, discard)
		defer g.stop()
		s.CloseConnections()
		in := &ingressConn{}
		s.handle(in, &egress{}, g)
		in.wantRefused(t, errUpstreamUnavailable)
		if st := s.Status(); st.Failed != 0 || g.confirming.Load() {
			t.Errorf("a dial the stitch cancelled counted as an egress failure (failed %d, probing %v)", st.Failed, g.confirming.Load())
		}
	})
	t.Run("loop", func(t *testing.T) {
		s := testStitch(config.AppDataAnnotate)
		in := &ingressConn{appData: []byte(`{"ziti_stitch":{"hops":8}}`)}
		s.handle(in, nil, nil) // refused before the egress side is used
		in.wantRefused(t, errLoopRefused)
	})
	t.Run("app data annotate cannot carry", func(t *testing.T) {
		s := testStitch(config.AppDataAnnotate)
		in := &ingressConn{appData: []byte(`{"ziti_stitch":{"dialer_identity_id":"ADMIN"}} x`)}
		s.handle(in, nil, nil) // refused before the egress side is used
		in.wantRefused(t, errAppDataRefused)
	})
}

// A connection that goes through: the egress side gets the annotated app
// data, bytes flow both ways and are counted, and the connection is listed
// while open.
func TestHandleSplicesAnAcceptedConnection(t *testing.T) {
	s := testStitch(config.AppDataAnnotate)
	g := newGate(&fakeListener{}, boundAt(edge.PrecedenceDefault), neverBack, discard)
	defer g.stop()
	client, in := tcpPair(t)
	out, server := tcpPair(t)
	e := &egress{conn: &tcpEdge{tcp: out, circuit: "circB"}}
	done := make(chan struct{})
	go func() {
		s.handle(&tcpEdge{tcp: in, circuit: "circ", appData: []byte(`{"dst_port":"443"}`)}, e, g)
		close(done)
	}()

	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = client.CloseWrite()
	if got, err := io.ReadAll(server); err != nil || string(got) != "ping" {
		t.Fatalf("egress side read %q, %v", got, err)
	}
	if want := `{"dst_port":"443","ziti_stitch":{"stitch":"s","dialer_identity_id":"id","dialer_identity_name":"client","circuit_id":"circ","hops":1}}`; string(e.probed) != want {
		t.Errorf("egress app data = %s, want %s", e.probed, want)
	}
	if c := s.Connections(); len(c) != 1 || c[0].DialerIdentityName != "client" || c[0].BCircuit != "circB" {
		t.Errorf("open connections = %+v, want the one from client", c)
	}
	if _, err := server.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	_ = server.CloseWrite()
	if got, err := io.ReadAll(client); err != nil || string(got) != "pong" {
		t.Fatalf("ingress side read %q, %v", got, err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handle did not return after both sides finished")
	}
	st := s.Status()
	if st.Active != 0 || st.Accepted != 1 || st.Failed != 0 || st.BytesToEgress != 4 || st.BytesToIngress != 4 {
		t.Errorf("status after the connection = %+v, want 1 accepted, none open or failed, 4 bytes each way", st)
	}
}

// The stitch's idle timeout reaches the splice.
func TestHandleClosesAnIdleConnection(t *testing.T) {
	s := testStitch(config.AppDataPassthrough)
	s.cfg.IdleTimeout = 50 * time.Millisecond
	g := newGate(&fakeListener{}, boundAt(edge.PrecedenceDefault), neverBack, discard)
	defer g.stop()
	client, in := tcpPair(t)
	out, _ := tcpPair(t)
	done := make(chan struct{})
	go func() {
		s.handle(&tcpEdge{tcp: in}, &egress{conn: &tcpEdge{tcp: out}}, g)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an idle connection was not closed")
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("ingress side read %v after the idle timeout, want EOF", err)
	}
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
