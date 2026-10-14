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

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/netfoundry/ziti-stitch/internal/network"
	"github.com/netfoundry/ziti-stitch/internal/runner"
	"github.com/netfoundry/ziti-stitch/internal/stitch"
)

func TestNotReady(t *testing.T) {
	ready := runner.Snapshot{
		Networks: []network.Status{{Name: "acme", Ready: true}, {Name: "partner", Ready: true}},
		Stitches: []stitch.Status{{Name: "a", State: stitch.StateListening}, {Name: "b", State: stitch.StateGated}},
	}
	if why := notReady(ready, true); why != "" {
		t.Errorf("listening and gated stitches on ready networks: %q", why)
	}
	if why := notReady(ready, false); !strings.Contains(why, "configuration") {
		t.Errorf("invalid config: %q", why)
	}
	netDown := ready
	netDown.Networks = []network.Status{{Name: "acme", Ready: true}, {Name: "partner"}}
	if why := notReady(netDown, true); why != "1 of 2 networks not ready" {
		t.Errorf("a network down: %q", why)
	}
	binding := ready
	binding.Stitches = []stitch.Status{{Name: "a", State: stitch.StateListening}, {Name: "b", State: stitch.StateBinding}}
	if why := notReady(binding, true); why != "1 of 2 stitches not listening" {
		t.Errorf("a stitch binding: %q", why)
	}
	if why := notReady(runner.Snapshot{}, true); why != "" {
		t.Errorf("nothing configured is ready: %q", why)
	}
}

func TestHealthHandler(t *testing.T) {
	snap := runner.Snapshot{Networks: []network.Status{{Name: "secret-name"}}}
	h := healthHandler(func() runner.Snapshot { return snap }, func() bool { return true })
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	if w := get("/healthz"); w.Code != http.StatusOK {
		t.Errorf("/healthz: %d", w.Code)
	}
	w := get("/readyz")
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "secret-name") {
		t.Errorf("/readyz with a network down: %d %q (names must not leak)", w.Code, w.Body.String())
	}
	snap.Networks[0].Ready = true
	if w := get("/readyz"); w.Code != http.StatusOK {
		t.Errorf("/readyz when ready: %d", w.Code)
	}
}
