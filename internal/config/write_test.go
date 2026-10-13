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

package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSaveStitchesRoundTripsAndStaysShort(t *testing.T) {
	dir := writeDir(t, twoNetworks(""))
	d, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	d.Stitches = []Stitch{
		{
			Name:    "plain",
			From:    Ingress{Network: "acme", Service: "a", Precedence: PrecedenceDefault},
			To:      Egress{Network: "partner", Service: "b", ConnectTimeout: DefaultConnectTimeout},
			AppData: AppDataPassthrough,
		},
		{
			Name:        "tuned",
			From:        Ingress{Network: "acme", Service: "c", Precedence: PrecedenceRequired, Cost: 5},
			To:          Egress{Network: "partner", Service: "d", ConnectTimeout: 2 * time.Second},
			AppData:     AppDataAnnotate,
			IdleTimeout: time.Hour,
			Disabled:    true,
		},
	}
	if err := d.SaveStitches(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, StitchesFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, defaultValue := range []string{"passthrough", "precedence: default", "5s"} {
		if strings.Contains(string(raw), defaultValue) {
			t.Errorf("saved file spells out the default %q:\n%s", defaultValue, raw)
		}
	}
	for _, set := range []string{"precedence: required", "cost: 5", "connect_timeout: 2s", "app_data: annotate", "idle_timeout: 1h0m0s", "disabled: true"} {
		if !strings.Contains(string(raw), set) {
			t.Errorf("saved file lacks %q:\n%s", set, raw)
		}
	}

	again, err := Load(dir)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if !slices.Equal(again.Stitches, d.Stitches) {
		t.Errorf("round trip changed the stitches:\n got %+v\nwant %+v", again.Stitches, d.Stitches)
	}
}

func TestReadDoesNotValidate(t *testing.T) {
	dir := writeDir(t, twoNetworks(`
stitches:
  - {name: there, from: {network: acme, service: a}, to: {network: partner, service: b}}
  - {name: back, from: {network: partner, service: b}, to: {network: acme, service: a}}
`))
	d, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if err := d.Validate(); err == nil {
		t.Fatal("Validate accepted a loop")
	}
	d.Stitches = d.Stitches[:1]
	if err := d.Validate(); err != nil {
		t.Fatalf("Validate after fixing the loop: %v", err)
	}
}

func TestEnsureDirIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, NetworksDirName))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != dirPerm {
		t.Errorf("networks dir mode = %v", fi.Mode())
	}
	if err := EnsureDir(dir); err != nil {
		t.Errorf("second EnsureDir: %v", err)
	}
}

func TestLookups(t *testing.T) {
	d, err := Read(writeDir(t, twoNetworks(`
stitches:
  - {name: one, from: {network: acme, service: a}, to: {network: partner, service: b}}
  - {name: two, from: {network: partner, service: c}, to: {network: partner, service: d}, disabled: true}
`)))
	if err != nil {
		t.Fatal(err)
	}
	if i := d.StitchIndex("two"); i != 1 {
		t.Errorf("StitchIndex(two) = %d", i)
	}
	if i := d.StitchIndex("nope"); i != -1 {
		t.Errorf("StitchIndex(nope) = %d", i)
	}
	if got := d.StitchesUsing("partner"); !slices.Equal(got, []string{"one", "two"}) {
		t.Errorf("StitchesUsing(partner) = %v", got)
	}
	if got := d.StitchesUsing("acme"); !slices.Equal(got, []string{"one"}) {
		t.Errorf("StitchesUsing(acme) = %v", got)
	}
	if got := d.IdentityPath("acme"); got != filepath.Join(d.Path, "networks", "acme.json") {
		t.Errorf("IdentityPath = %s", got)
	}
}

func TestSuggestName(t *testing.T) {
	tests := map[string]string{
		"https://ctrl.acme.example.com:1280/edge/client/v1":                         "acme",
		"https://acme.example.com":                                                  "acme",
		"https://ziti-ctrl.partner.io":                                              "ziti-ctrl",
		"https://controller.edge.globex.net":                                        "globex",
		"https://localhost:1280":                                                    "localhost",
		"https://c27bc9f6-6f43-4a8d-8a1b-1234567890ab.production.netfoundry.io:443": "production",
		"https://ctrl.example.com":                                                  "example",
	}
	for in, want := range tests {
		if got, ok := SuggestName(in); !ok || got != want {
			t.Errorf("SuggestName(%s) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"https://10.0.0.5:1280", "https://[::1]:1280", "https://ctrl.com", "::not a url", ""} {
		if got, ok := SuggestName(in); ok {
			t.Errorf("SuggestName(%s) = %q, want none", in, got)
		}
	}
}
