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

// Stitch is one configured stitch at run time.
type Stitch struct {
	cfg      config.Stitch
	from, to *network.Network
	log      *slog.Logger

	conns sync.WaitGroup // connections in flight, for draining

	// lastAppData is the app data of the most recent failed egress dial.
	// The gate's probe dials with it, because a tunneler host on the
	// egress side may reject a dial that carries none.
	lastAppData atomic.Pointer[[]byte]
}

// New returns a stitch from cfg between the given networks.
func New(cfg config.Stitch, from, to *network.Network, log *slog.Logger) *Stitch {
	return &Stitch{
		cfg:  cfg,
		from: from,
		to:   to,
		log: log.With("stitch", cfg.Name,
			"from", from.Name()+":"+cfg.From.Service,
			"to", to.Name()+":"+cfg.To.Service),
	}
}

// Run binds the ingress service and serves connections until ctx is done,
// rebinding with backoff whenever the bind fails or the listener closes
// (lost permission, removed service, unreachable routers). It returns once
// the listener is closed; connections still in flight carry on, see Wait.
func (s *Stitch) Run(ctx context.Context) {
	zA, err := s.from.Context(ctx)
	if err != nil {
		return
	}
	zB, err := s.to.Context(ctx)
	if err != nil {
		return
	}

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
		l, err := zA.ListenWithOptions(s.cfg.From.Service, s.listenOptions())
		if err != nil {
			wait := b.Next()
			s.log.Warn("cannot bind the ingress service; retrying", "error", err, "retry_in", wait)
			if !retry.Sleep(ctx, wait) {
				return
			}
			continue
		}

		s.log.Info("listening", "precedence", s.cfg.From.Precedence, "cost", s.cfg.From.Cost)
		started := time.Now()
		err = s.serve(ctx, l, zB)
		_ = l.Close()
		if ctx.Err() != nil {
			s.log.Info("stopped listening")
			return
		}
		if time.Since(started) >= stableListener {
			b.Reset()
		}
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

func (s *Stitch) listenOptions() *ziti.ListenOptions {
	opts := ziti.DefaultListenOptions()
	// We answer each dial ourselves, after the egress dial: see handle.
	opts.ManualStart = true
	opts.Precedence = ziti.GetPrecedenceForLabel(s.cfg.From.Precedence)
	opts.Cost = s.cfg.From.Cost
	opts.ConnectTimeout = bindTimeout
	opts.WaitForNEstablishedListeners = 1
	return opts
}

// serve accepts connections until the listener fails or ctx is done.
func (s *Stitch) serve(ctx context.Context, l edge.Listener, zB ziti.Context) error {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = l.Close() // unblocks AcceptEdge
		case <-stop:
		}
	}()

	g := newGate(l, edge.Precedence(ziti.GetPrecedenceForLabel(s.cfg.From.Precedence)),
		func(ctx context.Context) bool { return s.egressReachable(ctx, zB) }, s.log)
	defer g.stop()

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
		in.CompleteAcceptFailed(errLoopRefused)
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
		in.CompleteAcceptFailed(errUpstreamUnavailable)
		s.lastAppData.Store(&appData)
		g.failed()
		return
	}
	g.ok()
	log = log.With("b_circuit", out.GetCircuitId())

	if err := in.CompleteAcceptSuccess(); err != nil {
		_ = out.Close()
		log.Warn("connection failed: the ingress dialer went away before it completed", "error", err)
		return
	}
	log.Info("connection opened", "dial_time", time.Since(start))

	aToB, bToA, err := splice(in, out, s.cfg.IdleTimeout)
	attrs := []any{"bytes_to_egress", aToB, "bytes_to_ingress", bToA, "duration", time.Since(start)}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	log.Info("connection closed", attrs...)
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
	if p := s.lastAppData.Load(); p != nil {
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
