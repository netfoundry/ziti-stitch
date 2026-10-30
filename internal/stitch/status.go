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
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// State is what a stitch is doing.
type State string

const (
	StateWaiting   State = "waiting"   // for its networks to authenticate
	StateBinding   State = "binding"   // binding the ingress service, or retrying after a failure
	StateListening State = "listening" // serving
	StateGated     State = "gated"     // listening, but the egress service is unreachable; precedence lowered
	StateStopped   State = "stopped"
)

// Status is a stitch's state and counters, for display.
type Status struct {
	Name, From, To string
	State          State
	Since          time.Time // when State began
	// Err is the most recent problem (a failed bind, a failed egress dial),
	// kept with its time after things recover.
	Err   string
	ErrAt time.Time
	// Accepted counts ingress dials; Failed those refused because the
	// egress side could not be reached (or the connection looped).
	Active, Accepted, Failed      int64
	BytesToEgress, BytesToIngress int64 // closed connections plus open ones so far
}

// ConnStatus is one open connection, for display.
type ConnStatus struct {
	Stitch                               string
	DialerIdentityID, DialerIdentityName string
	ACircuit, BCircuit                   string
	Opened                               time.Time
	BytesToEgress, BytesToIngress        int64
}

// tracker keeps a stitch's status. Its zero value is ready to use.
type tracker struct {
	mu           sync.Mutex
	state        State
	since        time.Time
	err          string
	errAt        time.Time
	accepted     int64
	failed       int64
	closedEgress int64 // bytes of connections that have closed
	closedIngr   int64
	nextID       uint64
	open         map[uint64]*liveConn
	closing      bool // closeAll ran: close what opens from now on too
}

// liveConn is an open connection; splice counts into its byte counters.
type liveConn struct {
	info                ConnStatus
	toEgress, toIngress atomic.Int64
	close               func() // closes both sides
}

func (t *tracker) setState(s State) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != s {
		t.state, t.since = s, time.Now()
	}
}

func (t *tracker) problem(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.err, t.errAt = err.Error(), time.Now()
}

func (t *tracker) acceptedOne() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.accepted++
}

func (t *tracker) failedOne(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failed++
	t.err, t.errAt = err.Error(), time.Now()
}

// opened registers an open connection, which close closes, and returns it
// with the id to report its end by. After closeAll it is closed at once.
func (t *tracker) opened(info ConnStatus, close func()) (uint64, *liveConn) {
	t.mu.Lock()
	if t.open == nil {
		t.open = map[uint64]*liveConn{}
	}
	t.nextID++
	id := t.nextID // read under mu: another opened may move nextID on
	c := &liveConn{info: info, close: close}
	t.open[id] = c
	closing := t.closing
	t.mu.Unlock()
	if closing {
		close()
	}
	return id, c
}

// closeAll closes every open connection, and any opened later, and
// returns how many were open.
func (t *tracker) closeAll() int {
	t.mu.Lock()
	t.closing = true
	open := make([]*liveConn, 0, len(t.open))
	for _, c := range t.open {
		open = append(open, c)
	}
	t.mu.Unlock()
	for _, c := range open {
		c.close()
	}
	return len(open)
}

func (t *tracker) closed(id uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, ok := t.open[id]; ok {
		t.closedEgress += c.toEgress.Load()
		t.closedIngr += c.toIngress.Load()
		delete(t.open, id)
	}
}

func (t *tracker) status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := Status{
		State: t.state, Since: t.since, Err: t.err, ErrAt: t.errAt,
		Active: int64(len(t.open)), Accepted: t.accepted, Failed: t.failed,
		BytesToEgress: t.closedEgress, BytesToIngress: t.closedIngr,
	}
	if st.State == "" {
		st.State = StateWaiting
	}
	for _, c := range t.open {
		st.BytesToEgress += c.toEgress.Load()
		st.BytesToIngress += c.toIngress.Load()
	}
	return st
}

// connections returns the open connections, oldest first.
func (t *tracker) connections() []ConnStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ConnStatus, 0, len(t.open))
	for _, c := range t.open {
		cs := c.info
		cs.BytesToEgress, cs.BytesToIngress = c.toEgress.Load(), c.toIngress.Load()
		out = append(out, cs)
	}
	slices.SortFunc(out, func(a, b ConnStatus) int { return a.Opened.Compare(b.Opened) })
	return out
}
