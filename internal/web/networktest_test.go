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
	"sync/atomic"
	"testing"
	"time"

	"github.com/netfoundry/ziti-stitch/internal/diagnose"
)

func TestNetworkTestsAreSharedAndReused(t *testing.T) {
	var runs atomic.Int32
	release := make(chan struct{})
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &Server{
		probe: func(name, _ string) diagnose.NetworkReport {
			runs.Add(1)
			<-release
			return diagnose.NetworkReport{Name: name}
		},
		now: func() time.Time { return clock },
	}

	// Requests that arrive while a test runs wait for it rather than
	// start their own.
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() { s.testNetwork("acme", "acme.json") })
	}
	for runs.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	if n := runs.Load(); n != 1 {
		t.Fatalf("%d tests ran for 5 concurrent requests, want 1", n)
	}

	s.testNetwork("acme", "acme.json")
	if n := runs.Load(); n != 1 {
		t.Errorf("a fresh result was not reused: %d runs", n)
	}
	s.testNetwork("partner", "partner.json")
	if n := runs.Load(); n != 2 {
		t.Errorf("another network shared acme's result: %d runs", n)
	}
	clock = clock.Add(testResultFor)
	s.testNetwork("acme", "acme.json")
	if n := runs.Load(); n != 3 {
		t.Errorf("a stale result was reused: %d runs", n)
	}
}
