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
	"strings"
	"testing"
	"time"
)

// writeDir creates a config directory from a map of relative path to
// content and returns its path.
func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// twoNetworks has identities for networks "acme" and "partner".
func twoNetworks(stitches string) map[string]string {
	return map[string]string{
		"networks/acme.json":    "{}",
		"networks/partner.json": "{}",
		StitchesFileName:        stitches,
	}
}

func TestLoadEmptyDirectoryUsesDefaults(t *testing.T) {
	d, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d.Config.Log.Level != "info" || d.Config.Log.Format != "text" {
		t.Errorf("log = %+v, want info/text", d.Config.Log)
	}
	if len(d.Networks) != 0 || len(d.Stitches) != 0 {
		t.Errorf("got %d networks and %d stitches, want none", len(d.Networks), len(d.Stitches))
	}
}

func TestLoadStitchWithDefaults(t *testing.T) {
	dir := writeDir(t, twoNetworks(`
stitches:
  - name: billing
    from: {network: acme, service: billing-api}
    to: {network: partner, service: billing}
`))
	d, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(d.Stitches) != 1 {
		t.Fatalf("got %d stitches, want 1", len(d.Stitches))
	}
	s := d.Stitches[0]
	if s.AppData != AppDataPassthrough {
		t.Errorf("app_data = %q, want passthrough", s.AppData)
	}
	if s.From.Precedence != PrecedenceDefault {
		t.Errorf("precedence = %q, want default", s.From.Precedence)
	}
	if s.To.ConnectTimeout != DefaultConnectTimeout {
		t.Errorf("connect_timeout = %v, want %v", s.To.ConnectTimeout, DefaultConnectTimeout)
	}
	if s.IdleTimeout != 0 {
		t.Errorf("idle_timeout = %v, want 0", s.IdleTimeout)
	}
}

func TestLoadParsesEverything(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"networks/acme.json":    "{}",
		"networks/partner.json": "{}",
		ConfigFileName:          "log:\n  level: debug\n  format: json\n",
		StitchesFileName: `
stitches:
  - name: billing
    from: {network: acme, service: billing-api, precedence: required, cost: 10}
    to: {network: partner, service: billing, connect_timeout: 2500ms}
    app_data: annotate
    idle_timeout: 1h
    disabled: true
`,
	})
	d, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d.Config.Log != (Log{Level: "debug", Format: "json"}) {
		t.Errorf("log = %+v", d.Config.Log)
	}
	want := Stitch{
		Name:        "billing",
		From:        Ingress{Network: "acme", Service: "billing-api", Precedence: PrecedenceRequired, Cost: 10},
		To:          Egress{Network: "partner", Service: "billing", ConnectTimeout: 2500 * time.Millisecond},
		AppData:     AppDataAnnotate,
		IdleTimeout: time.Hour,
		Disabled:    true,
	}
	if d.Stitches[0] != want {
		t.Errorf("stitch = %+v\nwant     %+v", d.Stitches[0], want)
	}
	n, ok := d.Network("partner")
	if !ok || n.IdentityFile != filepath.Join(dir, NetworksDirName, "partner.json") {
		t.Errorf("Network(partner) = %+v, %v", n, ok)
	}
}

func TestLoadIgnoresTempAndPendingFiles(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"networks/acme.json":              "{}",
		"networks/acme.json.pending":      "{}",
		"networks/.acme.json.tmp-123":     "{}",
		"networks/README.txt":             "notes",
		"networks/partner.json":           "{}",
		"networks/subdir/ignored.json":    "{}",
		"networks/partner.json.tmp-other": "{}",
	})
	d, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var names []string
	for _, n := range d.Networks {
		names = append(names, n.Name)
	}
	if got := strings.Join(names, ","); got != "acme,partner" {
		t.Errorf("networks = %s, want acme,partner", got)
	}
}

func TestLoadRejectsBadNetworkFileName(t *testing.T) {
	dir := writeDir(t, map[string]string{"networks/Acme Corp.json": "{}"})
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "not a valid network name") {
		t.Fatalf("err = %v, want a network name error", err)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	dir := writeDir(t, twoNetworks(`
stitches:
  - name: billing
    from: {network: acme, service: billing-api}
    to: {network: partner, service: billing}
    app-data: strip
`))
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "app-data") {
		t.Fatalf("err = %v, want an unknown field error naming app-data", err)
	}
}

func TestLoadRejectsMissingDirectory(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name     string
		stitches string
		want     []string
	}{
		{
			name:     "missing fields",
			stitches: "stitches:\n  - from: {}\n    to: {}\n",
			want:     []string{"stitch #1 has no name", "from.network is required", "from.service is required", "to.network is required", "to.service is required"},
		},
		{
			name: "bad values",
			stitches: `
stitches:
  - name: Billing
    from: {network: acme, service: a, precedence: high}
    to: {network: nowhere, service: b, connect_timeout: -1s}
    app_data: copy
    idle_timeout: -5s
`,
			want: []string{"name is not valid", `to.network "nowhere" has no identity`, `precedence "high"`, `app_data "copy"`, "idle_timeout cannot be negative", "connect_timeout cannot be negative"},
		},
		{
			name: "duplicate name",
			stitches: `
stitches:
  - {name: s, from: {network: acme, service: a}, to: {network: partner, service: b}}
  - {name: s, from: {network: acme, service: c}, to: {network: partner, service: d}}
`,
			want: []string{"name is used more than once"},
		},
		{
			name: "same ingress twice",
			stitches: `
stitches:
  - {name: one, from: {network: acme, service: a}, to: {network: partner, service: b}}
  - {name: two, from: {network: acme, service: a}, to: {network: partner, service: c}}
`,
			want: []string{`binds acme:a, which stitch "one" already binds`},
		},
		{
			name:     "dials itself",
			stitches: "stitches:\n  - {name: s, from: {network: acme, service: a}, to: {network: acme, service: a}}\n",
			want:     []string{"dials the service it binds"},
		},
		{
			name: "two-stitch loop",
			stitches: `
stitches:
  - {name: there, from: {network: acme, service: a}, to: {network: partner, service: b}}
  - {name: back, from: {network: partner, service: b}, to: {network: acme, service: a}}
`,
			want: []string{"stitches form a loop: there -> back -> there"},
		},
		{
			name: "three-stitch loop",
			stitches: `
stitches:
  - {name: one, from: {network: acme, service: a}, to: {network: partner, service: b}}
  - {name: two, from: {network: partner, service: b}, to: {network: acme, service: c}}
  - {name: three, from: {network: acme, service: c}, to: {network: acme, service: a}}
`,
			want: []string{"stitches form a loop: one -> two -> three -> one"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeDir(t, twoNetworks(tt.stitches)))
			if err == nil {
				t.Fatal("expected validation errors")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not mention %q:\n%v", w, err)
				}
			}
		})
	}
}

func TestLoopReportedOnce(t *testing.T) {
	_, err := Load(writeDir(t, twoNetworks(`
stitches:
  - {name: there, from: {network: acme, service: a}, to: {network: partner, service: b}}
  - {name: back, from: {network: partner, service: b}, to: {network: acme, service: a}}
`)))
	if err == nil {
		t.Fatal("expected a loop error")
	}
	if n := strings.Count(err.Error(), "form a loop"); n != 1 {
		t.Errorf("loop reported %d times, want once:\n%v", n, err)
	}
}

func TestValidConfigurationsPass(t *testing.T) {
	tests := map[string]string{
		"chain is not a loop": `
stitches:
  - {name: one, from: {network: acme, service: a}, to: {network: partner, service: b}}
  - {name: two, from: {network: partner, service: b}, to: {network: partner, service: c}}
`,
		"disabled stitch breaks the loop": `
stitches:
  - {name: there, from: {network: acme, service: a}, to: {network: partner, service: b}}
  - {name: back, from: {network: partner, service: b}, to: {network: acme, service: a}, disabled: true}
`,
		"disabled stitch may share an ingress": `
stitches:
  - {name: one, from: {network: acme, service: a}, to: {network: partner, service: b}}
  - {name: two, from: {network: acme, service: a}, to: {network: partner, service: c}, disabled: true}
`,
		"same network, different services": `
stitches:
  - {name: alias, from: {network: acme, service: new-name}, to: {network: acme, service: old-name}}
`,
	}
	for name, stitches := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeDir(t, twoNetworks(stitches))); err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}
}

func TestValidName(t *testing.T) {
	for _, s := range []string{"acme", "a", "partner-2", "us_east", "0net", strings.Repeat("a", 63)} {
		if !ValidName(s) {
			t.Errorf("ValidName(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "Acme", "-acme", "_x", "a b", "a.b", "a/b", strings.Repeat("a", 64)} {
		if ValidName(s) {
			t.Errorf("ValidName(%q) = true, want false", s)
		}
	}
}

func TestDefaultDirHonorsEnv(t *testing.T) {
	t.Setenv(EnvConfigDir, "/tmp/somewhere")
	d, err := DefaultDir()
	if err != nil || d != "/tmp/somewhere" {
		t.Fatalf("DefaultDir() = %q, %v", d, err)
	}
}

func TestWebListenMustBeLoopback(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8474", "[::1]:8474", "localhost:9000", "127.0.0.2:80"} {
		dir := writeDir(t, map[string]string{ConfigFileName: "web:\n  listen: \"" + ok + "\"\n"})
		if _, err := Load(dir); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:8474", "192.168.1.5:8474", "example.com:80", "127.0.0.1", "127.0.0.1:http", "127.0.0.1:70000"} {
		dir := writeDir(t, map[string]string{ConfigFileName: "web:\n  listen: \"" + bad + "\"\n"})
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "web.listen") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
}
