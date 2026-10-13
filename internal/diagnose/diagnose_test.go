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

package diagnose

import (
	"errors"
	"strings"
	"testing"

	"github.com/openziti/edge-api/rest_model"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

func stitch(name, from, fromSvc, to, toSvc string) config.Stitch {
	return config.Stitch{
		Name: name,
		From: config.Ingress{Network: from, Service: fromSvc},
		To:   config.Egress{Network: to, Service: toSvc},
	}
}

func TestStitches(t *testing.T) {
	reports := map[string]NetworkReport{
		"acme": {Name: "acme", IdentityName: "stitch-a", Services: map[string]Access{
			"billing":  {Bind: true},
			"dialonly": {Dial: true},
		}},
		"partner": {Name: "partner", IdentityName: "stitch-b", Services: map[string]Access{
			"billing":  {Dial: true},
			"bindonly": {Bind: true},
		}},
		"down": {Name: "down", Err: errors.New("connection refused")},
	}
	tests := []struct {
		stitch config.Stitch
		want   []string // one substring per finding, in order; "" means OK
	}{
		{stitch("good", "acme", "billing", "partner", "billing"), []string{"", ""}},
		{stitch("nobind", "acme", "dialonly", "partner", "billing"), []string{`identity "stitch-a" can dial acme:dialonly but not bind it`, ""}},
		{stitch("nodial", "acme", "billing", "partner", "bindonly"), []string{"", `can bind partner:bindonly but not dial it`}},
		{stitch("invisible", "acme", "ghost", "partner", "billing"), []string{`acme:ghost is not visible to identity "stitch-a"`, ""}},
		{stitch("unreachable", "acme", "billing", "down", "x"), []string{"", "network down did not authenticate: connection refused"}},
	}
	for _, tt := range tests {
		got := Stitches([]config.Stitch{tt.stitch}, reports)
		if len(got) != len(tt.want) {
			t.Fatalf("%s: %d findings, want %d", tt.stitch.Name, len(got), len(tt.want))
		}
		for i, w := range tt.want {
			f := got[i]
			if f.Stitch != tt.stitch.Name {
				t.Errorf("%s: finding names stitch %q", tt.stitch.Name, f.Stitch)
			}
			if w == "" {
				if f.Severity != OK {
					t.Errorf("%s #%d: %s", tt.stitch.Name, i, f.Message)
				}
				continue
			}
			if f.Severity != Problem || !strings.Contains(f.Message, w) {
				t.Errorf("%s #%d = %v %q, want a problem mentioning %q", tt.stitch.Name, i, f.Severity, f.Message, w)
			}
		}
	}
}

func TestStitchesSkipsDisabled(t *testing.T) {
	s := stitch("off", "acme", "x", "partner", "y")
	s.Disabled = true
	if got := Stitches([]config.Stitch{s}, nil); len(got) != 0 {
		t.Errorf("findings for a disabled stitch: %v", got)
	}
}

func TestAccessOf(t *testing.T) {
	if a := accessOf([]rest_model.DialBind{rest_model.DialBindBind}); a != (Access{Bind: true}) {
		t.Errorf("accessOf(Bind) = %+v", a)
	}
	if a := accessOf([]rest_model.DialBind{rest_model.DialBindDial, rest_model.DialBindBind}); a != (Access{Dial: true, Bind: true}) {
		t.Errorf("accessOf(Dial, Bind) = %+v", a)
	}
	if a := accessOf(nil); a != (Access{}) {
		t.Errorf("accessOf(nil) = %+v", a)
	}
}

func TestStitchesSkipsUnreportedNetworks(t *testing.T) {
	reports := map[string]NetworkReport{
		"acme": {Name: "acme", Services: map[string]Access{"a": {Bind: true}}},
	}
	got := Stitches([]config.Stitch{stitch("s", "acme", "a", "partner", "b")}, reports)
	if len(got) != 1 || got[0].Severity != OK || !strings.Contains(got[0].Message, "acme:a") {
		t.Errorf("findings = %+v, want only the acme end", got)
	}
}
