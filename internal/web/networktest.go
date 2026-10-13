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

package web

import (
	"sync"
	"time"

	"github.com/netfoundry/ziti-stitch/internal/diagnose"
)

// testResultFor is how long a network test's result is reused. Each test
// logs in to the network's controller, and anyone with read access can ask
// for one, so repeated requests must not turn into a stream of logins.
// Short enough that rerunning a test after a policy change soon shows it.
const testResultFor = 10 * time.Second

// networkTests runs at most one test per network at a time, and shares a
// result with every request that asks within testResultFor of it.
type networkTests struct {
	mu      sync.Mutex
	results map[string]*networkTestResult // by network name and identity file
}

type networkTestResult struct {
	done   chan struct{} // closed once report and at are set
	report diagnose.NetworkReport
	at     time.Time
}

// testNetwork returns the report for the network name, running probe if
// there is no recent one and none is under way.
func (s *Server) testNetwork(name, path string) diagnose.NetworkReport {
	t := &s.tests
	key := name + "\x00" + path
	t.mu.Lock()
	r, ok := t.results[key]
	if !ok || r.stale(s.now()) {
		r = &networkTestResult{done: make(chan struct{})}
		if t.results == nil {
			t.results = map[string]*networkTestResult{}
		}
		t.results[key] = r
		t.mu.Unlock()
		r.report, r.at = s.probe(name, path), s.now()
		close(r.done)
		return r.report
	}
	t.mu.Unlock()
	<-r.done
	return r.report
}

// stale reports whether a finished result is too old to reuse. One still
// running is not.
func (r *networkTestResult) stale(now time.Time) bool {
	select {
	case <-r.done:
		return now.Sub(r.at) >= testResultFor
	default:
		return false
	}
}
