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

package network

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/sdk-golang/ziti/edge"
)

// hostListener stands in for the SDK's listener; established says how
// many terminators a router has confirmed.
type hostListener struct {
	edge.Listener
	established atomic.Uint32
	closed      atomic.Bool
}

func (l *hostListener) GetEstablishedCount() uint { return uint(l.established.Load()) }
func (l *hostListener) IsClosed() bool            { return l.closed.Load() }
func (l *hostListener) Close() error              { l.closed.Store(true); return nil }

type bindContext struct {
	ziti.Context
	l    edge.Listener
	opts ziti.ListenOptions
}

func (c *bindContext) ListenWithOptions(_ string, o *ziti.ListenOptions) (edge.Listener, error) {
	c.opts = *o
	return c.l, nil
}

func TestListenWaitsForTheRouter(t *testing.T) {
	l := &hostListener{}
	z := &bindContext{l: l}
	time.AfterFunc(2*establishedPoll, func() { l.established.Store(1) })
	got, err := Listen(context.Background(), z, "svc", &ziti.ListenOptions{WaitForNEstablishedListeners: 1}, 5*time.Second)
	if err != nil || got != l {
		t.Fatalf("Listen = %v, %v", got, err)
	}
	if z.opts.WaitForNEstablishedListeners != 0 {
		t.Error("the SDK's own wait was left on")
	}
	if l.closed.Load() {
		t.Error("an established listener was closed")
	}
}

func TestListenGivesUpAndCloses(t *testing.T) {
	l := &hostListener{}
	if _, err := Listen(context.Background(), &bindContext{l: l}, "svc", &ziti.ListenOptions{}, 3*establishedPoll); err == nil || !l.closed.Load() {
		t.Errorf("err = %v, closed = %v; want an error and the listener closed", err, l.closed.Load())
	}
}

func TestListenStopsWithTheContext(t *testing.T) {
	l := &hostListener{}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(establishedPoll, cancel)
	if _, err := Listen(ctx, &bindContext{l: l}, "svc", &ziti.ListenOptions{}, time.Minute); !errors.Is(err, context.Canceled) || !l.closed.Load() {
		t.Errorf("err = %v, closed = %v; want context.Canceled and the listener closed", err, l.closed.Load())
	}
}

func TestListenNoticesAClosedListener(t *testing.T) {
	l := &hostListener{}
	l.closed.Store(true)
	if _, err := Listen(context.Background(), &bindContext{l: l}, "svc", &ziti.ListenOptions{}, time.Minute); err == nil {
		t.Error("a listener that closed before it was established was returned")
	}
}
