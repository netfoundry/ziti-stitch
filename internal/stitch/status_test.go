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
	"errors"
	"testing"
	"time"
)

func TestTrackerStartsWaiting(t *testing.T) {
	var tr tracker
	if st := tr.status(); st.State != StateWaiting || st.Active != 0 {
		t.Errorf("status = %+v", st)
	}
}

func TestTrackerStateKeepsItsStartTime(t *testing.T) {
	var tr tracker
	tr.setState(StateListening)
	since := tr.status().Since
	time.Sleep(time.Millisecond)
	tr.setState(StateListening)
	if got := tr.status().Since; !got.Equal(since) {
		t.Errorf("setting the same state again moved Since from %v to %v", since, got)
	}
	tr.setState(StateBinding)
	if got := tr.status().Since; !got.After(since) {
		t.Error("a new state did not move Since")
	}
}

func TestTrackerCountsAndConnections(t *testing.T) {
	var tr tracker
	tr.acceptedOne()
	tr.acceptedOne()
	tr.acceptedOne()
	tr.failedOne(errors.New("egress down"))

	t0 := time.Now()
	id1, c1 := tr.opened(ConnStatus{Stitch: "s", ACircuit: "a1", Opened: t0}, func() {})
	_, c2 := tr.opened(ConnStatus{Stitch: "s", ACircuit: "a2", Opened: t0.Add(time.Second)}, func() {})
	c1.toEgress.Add(10)
	c1.toIngress.Add(20)
	c2.toEgress.Add(1)

	st := tr.status()
	if st.Accepted != 3 || st.Failed != 1 || st.Active != 2 || st.Err != "egress down" {
		t.Errorf("status = %+v", st)
	}
	if st.BytesToEgress != 11 || st.BytesToIngress != 20 {
		t.Errorf("bytes = %d/%d, want 11/20", st.BytesToEgress, st.BytesToIngress)
	}
	conns := tr.connections()
	if len(conns) != 2 || conns[0].ACircuit != "a1" || conns[0].BytesToIngress != 20 {
		t.Errorf("connections = %+v", conns)
	}

	tr.closed(id1)
	tr.closed(id1) // twice is harmless
	st = tr.status()
	if st.Active != 1 || st.BytesToEgress != 11 || st.BytesToIngress != 20 {
		t.Errorf("after close: %+v (closed bytes must stay counted)", st)
	}
}

// closeAll closes every open connection, and one that opens afterwards
// (its egress dial was already under way) at once.
func TestTrackerCloseAll(t *testing.T) {
	var tr tracker
	var closed []string
	closer := func(name string) func() { return func() { closed = append(closed, name) } }
	tr.opened(ConnStatus{ACircuit: "a1"}, closer("a1"))
	id, _ := tr.opened(ConnStatus{ACircuit: "a2"}, closer("a2"))
	tr.closed(id) // finished on its own

	if n := tr.closeAll(); n != 1 || len(closed) != 1 || closed[0] != "a1" {
		t.Errorf("closeAll = %d, closed %v; want 1, [a1]", n, closed)
	}
	tr.opened(ConnStatus{ACircuit: "a3"}, closer("a3"))
	if len(closed) != 2 || closed[1] != "a3" {
		t.Errorf("closed %v; a connection opened after closeAll stayed open", closed)
	}
}
