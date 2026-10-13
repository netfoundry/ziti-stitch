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

// Package stitch binds a service on one network and, for every connection
// that arrives, dials a service on another network and splices the two.
package stitch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/sdk-golang/ziti/edge"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/network"
	"github.com/netfoundry/ziti-stitch/internal/retry"
)

// RefusalPrefix starts every error we hand the SDK to fail an ingress dial.
const RefusalPrefix = "ziti-stitch: "

// What the ingress dialer sees when we fail its dial. The real reason is
// only logged here: it comes from the egress network (service ids and the
// like), which the ingress side has no business seeing.
var (
	errUpstreamUnavailable = errors.New(RefusalPrefix + "upstream service unavailable")
	errLoopRefused         = errors.New(RefusalPrefix + "refused, the connection is looping between stitches")
)

const (
	// bindTimeout is how long a bind waits for its first terminator before
	// counting as failed and being retried.
	bindTimeout = 10 * time.Second
	// stableListener is how long a listener must last before a failure
	// resets the rebind backoff.
	stableListener = time.Minute
)

// Network is where a stitch gets its Ziti contexts. *network.Network is
// the real one.
type Network interface {
	Name() string
	// Context waits until the network is ready or ctx is done.
	Context(ctx context.Context) (ziti.Context, error)
}

// Stitch is one configured stitch at run time.
type Stitch struct {
	cfg      config.Stitch
	from, to Network
	log      *slog.Logger

	conns sync.WaitGroup // connections in flight, for draining

	// probeAppData is what the gate's probe dials with, because a
	// tunneler host on the egress side may reject a dial that carries
	// none. It is the app data of the last egress dial that succeeded, so
	// one client whose app data the host rejects cannot keep the stitch
	// gated; until a dial has succeeded, that of the first failed one.
	probeAppData atomic.Pointer[[]byte]

	track tracker
	gate  atomic.Pointer[gate]  // the current listener's, while there is one
	terms atomic.Pointer[terms] // cost and precedence, changed live by Retune
}

// Status reports what the stitch is doing and its counters.
func (s *Stitch) Status() Status {
	st := s.track.status()
	st.Name = s.cfg.Name
	st.From = s.from.Name() + ":" + s.cfg.From.Service
	st.To = s.to.Name() + ":" + s.cfg.To.Service
	if g := s.gate.Load(); st.State == StateListening && g != nil && g.isDown() {
		st.State = StateGated
	}
	return st
}

// Connections returns the stitch's open connections, oldest first.
func (s *Stitch) Connections() []ConnStatus {
	return s.track.connections()
}

// New returns a stitch from cfg between the given networks.
func New(cfg config.Stitch, from, to Network, log *slog.Logger) *Stitch {
	s := &Stitch{
		cfg:  cfg,
		from: from,
		to:   to,
		log: log.With("stitch", cfg.Name,
			"from", from.Name()+":"+cfg.From.Service,
			"to", to.Name()+":"+cfg.To.Service),
	}
	s.terms.Store(termsOf(cfg.From))
	return s
}

func termsOf(in config.Ingress) *terms {
	return &terms{precedence: edge.Precedence(ziti.GetPrecedenceForLabel(in.Precedence)), cost: in.Cost}
}

// Retune changes the precedence and cost of the stitch's terminators
// without rebinding, so dials keep flowing while it happens. The stitch's
// other settings are fixed for its lifetime.
func (s *Stitch) Retune(in config.Ingress) {
	t := termsOf(in)
	s.terms.Store(t)
	s.log.Info("changing terminator precedence and cost", "precedence", in.Precedence, "cost", in.Cost)
	if g := s.gate.Load(); g != nil {
		if err := g.retune(*t); err != nil {
			s.log.Error("changing terminator precedence and cost failed; it applies from the next bind", "error", err)
		}
	}
}

// Run binds the ingress service and serves connections until ctx is done,
// rebinding with backoff whenever the bind fails or the listener closes
// (lost permission, removed service, unreachable routers). It returns once
// the listener is closed; connections still in flight carry on, see Wait.
func (s *Stitch) Run(ctx context.Context) {
	defer s.track.setState(StateStopped)
	s.track.setState(StateWaiting)
	zA, err := s.from.Context(ctx)
	if err != nil {
		return
	}
	zB, err := s.to.Context(ctx)
	if err != nil {
		return
	}
	s.track.setState(StateBinding)

	b := retry.Backoff{Min: time.Second, Max: 30 * time.Second}
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// The SDK's service list may predate the problem being fixed
			// (a restored Bind policy, say); without this a retry can keep
			// failing on the cached list until its next refresh.
			if _, err := zA.RefreshService(s.cfg.From.Service); err != nil {
				s.log.Debug("refreshing the ingress service failed", "error", err)
			}
		}
		bound := *s.terms.Load()
		l, err := network.Listen(ctx, zA, s.cfg.From.Service, listenOptions(bound), bindTimeout)
		if err != nil {
			s.track.problem(fmt.Errorf("cannot bind %s: %w", s.cfg.From.Service, err))
			wait := b.Next()
			s.log.Warn("cannot bind the ingress service; retrying", "error", err, "retry_in", wait)
			if !retry.Sleep(ctx, wait) {
				return
			}
			continue
		}

		s.log.Info("listening", "precedence", ziti.Precedence(bound.precedence).String(), "cost", bound.cost)
		s.track.setState(StateListening)
		started := time.Now()
		err = s.serve(ctx, l, zB, bound)
		_ = l.Close()
		if ctx.Err() != nil {
			s.log.Info("stopped listening")
			return
		}
		if time.Since(started) >= stableListener {
			b.Reset()
		}
		s.track.setState(StateBinding)
		s.track.problem(fmt.Errorf("listener closed: %w", err))
		wait := b.Next()
		s.log.Warn("listener closed; rebinding", "error", err, "retry_in", wait)
		if !retry.Sleep(ctx, wait) {
			return
		}
	}
}

// Wait blocks until every connection the stitch accepted has finished.
// Call it after Run has returned.
func (s *Stitch) Wait() {
	s.conns.Wait()
}

func listenOptions(t terms) *ziti.ListenOptions {
	opts := ziti.DefaultListenOptions()
	// We answer each dial ourselves, after the egress dial: see handle.
	opts.ManualStart = true
	opts.Precedence = ziti.Precedence(t.precedence)
	opts.Cost = t.cost
	opts.ConnectTimeout = bindTimeout
	return opts
}

// terminatorWatcher is implemented by the SDK's listener: it reports
// terminators being added and removed.
type terminatorWatcher interface {
	SetConnectionChangeHandler(func([]edge.RouterHostConn))
}

// serve accepts connections until the listener fails or ctx is done. bound
// is the precedence and cost l was created with.
func (s *Stitch) serve(ctx context.Context, l edge.Listener, zB ziti.Context, bound terms) error {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = l.Close() // unblocks AcceptEdge
		case <-stop:
		}
	}()

	g := newGate(l, bound, func(ctx context.Context) bool { return s.egressReachable(ctx, zB) }, s.log)
	s.gate.Store(g)
	defer func() {
		s.gate.Store(nil)
		g.stop()
	}()
	if w, ok := l.(terminatorWatcher); ok {
		w.SetConnectionChangeHandler(func([]edge.RouterHostConn) { g.terminatorsChanged() })
	} else {
		s.log.Warn("the SDK's listener does not report new terminators; ones bound while gated or after a cost or precedence change get the original values until the next probe")
	}
	// A Retune that ran before the gate existed.
	if cur := *s.terms.Load(); cur != bound {
		if err := g.retune(cur); err != nil {
			s.log.Error("changing terminator precedence and cost failed; it applies from the next bind", "error", err)
		}
	}

	for {
		in, err := l.AcceptEdge()
		if err != nil {
			return err
		}
		s.conns.Go(func() { s.handle(in, zB, g) })
	}
}

// handle dials the egress service for one accepted connection, answers the
// ingress dial accordingly, and splices the two until both are done.
//
// The listener uses ManualStart, so the ingress dialer is still waiting on
// its dial while we dial the egress side. If that fails, the dialer gets a
// dial failure rather than a connection that opens and drops at once.
func (s *Stitch) handle(in edge.Conn, zB ziti.Context, g *gate) {
	start := time.Now()
	s.track.acceptedOne()
	log := s.log.With(
		"a_circuit", in.GetCircuitId(),
		"dialer_identity_id", in.GetDialerIdentityId(),
		"dialer_identity_name", in.GetDialerIdentityName())

	appData, err := egressAppData(s.cfg.AppData, in.GetAppData(), s.cfg.Name, origin{
		dialerIdentityID:   in.GetDialerIdentityId(),
		dialerIdentityName: in.GetDialerIdentityName(),
		circuitID:          in.GetCircuitId(),
	})
	if errors.Is(err, errTooManyHops) {
		log.Warn("connection refused: it has passed through too many stitches, which suggests a loop")
		refuse(in, errLoopRefused)
		s.track.failedOne(errTooManyHops)
		return
	}
	if err != nil {
		log.Warn("cannot annotate app data; forwarding it unchanged", "error", err)
		appData = in.GetAppData()
	}

	out, err := zB.DialWithOptions(s.cfg.To.Service, &ziti.DialOptions{
		ConnectTimeout: s.cfg.To.ConnectTimeout,
		AppData:        appData,
	})
	if err != nil {
		log.Warn("connection failed: cannot reach the egress service", "error", err, "dial_time", time.Since(start))
		refuse(in, errUpstreamUnavailable)
		s.track.failedOne(fmt.Errorf("cannot reach %s: %w", s.cfg.To.Service, err))
		s.probeAppData.CompareAndSwap(nil, &appData)
		g.failed()
		return
	}
	s.probeAppData.Store(&appData)
	g.ok()
	log = log.With("b_circuit", out.GetCircuitId())

	if err := in.CompleteAcceptSuccess(); err != nil {
		_ = out.Close()
		log.Warn("connection failed: the ingress dialer went away before it completed", "error", err)
		return
	}
	log.Info("connection opened", "dial_time", time.Since(start))

	id, live := s.track.opened(ConnStatus{
		Stitch:             s.cfg.Name,
		DialerIdentityID:   in.GetDialerIdentityId(),
		DialerIdentityName: in.GetDialerIdentityName(),
		ACircuit:           in.GetCircuitId(),
		BCircuit:           out.GetCircuitId(),
		Opened:             start,
	})
	defer s.track.closed(id)
	err = splice(in, out, s.cfg.IdleTimeout, &live.toEgress, &live.toIngress)
	attrs := []any{"bytes_to_egress", live.toEgress.Load(), "bytes_to_ingress", live.toIngress.Load(), "duration", time.Since(start)}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	log.Info("connection closed", attrs...)
}

// refuse fails an ingress dial we accepted but cannot complete. The SDK
// registered the connection with the router channel before handing it to
// us, and CompleteAcceptFailed only answers the router, so it must be
// closed as well or it stays registered until the channel goes.
func refuse(in edge.Conn, reason error) {
	in.CompleteAcceptFailed(reason)
	_ = in.Close()
}

// egressReachable is the gate's probe. A service with no terminators is
// certainly down, and checking costs the egress side nothing. Otherwise
// only a real dial tells (a host can be bound while its backend is down);
// the far end sees a connection open and close without data.
func (s *Stitch) egressReachable(ctx context.Context, zB ziti.Context) bool {
	if terms, _, err := zB.GetServiceTerminators(s.cfg.To.Service, 0, 1); err == nil && len(terms) == 0 {
		return false
	}
	var appData []byte
	if p := s.probeAppData.Load(); p != nil {
		appData = *p
	}
	c, err := zB.DialContextWithOptions(ctx, s.cfg.To.Service, &ziti.DialOptions{
		ConnectTimeout: s.cfg.To.ConnectTimeout,
		AppData:        appData,
	})
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
