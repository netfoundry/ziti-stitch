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

func TestWebZiti(t *testing.T) {
	good := `
web:
  ziti:
    network: acme
    service: stitch-admin
    hosts: [stitch-admin.ziti]
    allow: [AbCd123]
`
	d, err := Load(writeDir(t, map[string]string{"networks/acme.json": "{}", ConfigFileName: good}))
	if err != nil {
		t.Fatal(err)
	}
	if z := d.Config.Web.Ziti; z.PolicyAccess != AccessNone || z.Allow[0] != "AbCd123" {
		t.Errorf("web.ziti = %+v", z)
	}

	tests := map[string]string{
		"web:\n  ziti:\n    service: s\n    hosts: [h]\n":                                            "web.ziti.network is required",
		"web:\n  ziti:\n    network: nope\n    service: s\n    hosts: [h]\n":                         `web.ziti.network "nope" has no identity`,
		"web:\n  ziti:\n    network: acme\n    hosts: [h]\n":                                         "web.ziti.service is required",
		"web:\n  ziti:\n    network: acme\n    service: s\n":                                         "web.ziti.hosts is required",
		"web:\n  ziti:\n    network: acme\n    service: s\n    hosts: [\"a b\"]\n":                   "is not a host name",
		"web:\n  ziti:\n    network: acme\n    service: s\n    hosts: [\"h:0\"]\n":                   "port must be a number",
		"web:\n  ziti:\n    network: acme\n    service: s\n    hosts: [\"h:x\"]\n":                   "port must be a number",
		"web:\n  ziti:\n    network: acme\n    service: s\n    hosts: [\":80\"]\n":                   "is not a host name",
		"web:\n  ziti:\n    network: acme\n    service: s\n    hosts: [h]\n    policy_access: all\n": `policy_access "all"`,
	}
	for cfg, want := range tests {
		_, err := Load(writeDir(t, map[string]string{"networks/acme.json": "{}", ConfigFileName: cfg}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", cfg, err, want)
		}
	}
}

func TestWebHostWithPort(t *testing.T) {
	cfg := "web:\n  ziti:\n    network: acme\n    service: s\n    hosts: [\"stitch-admin.ziti:8080\", h]\n"
	if _, err := Load(writeDir(t, map[string]string{"networks/acme.json": "{}", ConfigFileName: cfg})); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{"stitch-admin.ziti:8080": "stitch-admin.ziti", "h": "h"} {
		if got := HostName(in); got != want {
			t.Errorf("HostName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWebZitiServiceCannotBeAStitchIngress(t *testing.T) {
	files := map[string]string{
		"networks/acme.json":    "{}",
		"networks/partner.json": "{}",
		ConfigFileName:          "web:\n  ziti:\n    network: acme\n    service: billing-api\n    hosts: [h]\n",
		StitchesFileName:        "stitches:\n  - name: billing\n    from: {network: acme, service: billing-api}\n    to: {network: partner, service: billing}\n",
	}
	if _, err := Load(writeDir(t, files)); err == nil || !strings.Contains(err.Error(), `web.ziti binds acme:billing-api, which stitch "billing" also binds`) {
		t.Errorf("err = %v", err)
	}
}

func TestWebSameListeners(t *testing.T) {
	z := func(allow ...string) *WebZiti {
		return &WebZiti{Network: "acme", Service: "s", Hosts: []string{"h"}, Allow: allow, PolicyAccess: AccessNone}
	}
	base := Web{Listen: "127.0.0.1:1", Ziti: z()}
	if !base.SameListeners(Web{Listen: "127.0.0.1:1", Ziti: z("someone")}) {
		t.Error("an allowlist change should not need a restart")
	}
	for name, other := range map[string]Web{
		"listen":  {Listen: "127.0.0.1:2", Ziti: z()},
		"no ziti": {Listen: "127.0.0.1:1"},
		"service": {Listen: "127.0.0.1:1", Ziti: &WebZiti{Network: "acme", Service: "t", Hosts: []string{"h"}}},
		"hosts":   {Listen: "127.0.0.1:1", Ziti: &WebZiti{Network: "acme", Service: "s", Hosts: []string{"h", "i"}}},
	} {
		if base.SameListeners(other) {
			t.Errorf("%s change not seen", name)
		}
	}
	if !(Web{}).SameListeners(Web{}) {
		t.Error("two empty configs differ")
	}
}

func TestConfigSource(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"networks/acme.json": "{}", "networks/partner.json": "{}",
		StitchesFileName: "stitches:\n  - name: local\n    from: {network: acme, service: a}\n    to: {network: partner, service: b}\n",
	})
	src := writeDir(t, map[string]string{
		ConfigFileName:   "log:\n  level: debug\n",
		StitchesFileName: "stitches:\n  - name: managed\n    from: {network: acme, service: a}\n    to: {network: partner, service: b}\n",
	})
	t.Setenv(EnvConfigSource, src)

	d, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if d.Config.Log.Level != "debug" || len(d.Stitches) != 1 || d.Stitches[0].Name != "managed" || len(d.Networks) != 2 {
		t.Errorf("read %+v; want config and stitches from the source, networks from the config dir", d)
	}
	if err := d.Edit(func(*Dir) error { return nil }); err == nil || !strings.Contains(err.Error(), src) {
		t.Errorf("Edit = %v, want it refused, naming the source", err)
	}
	if err := d.SaveStitches(); err == nil {
		t.Error("SaveStitches wrote over a managed config")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, StitchesFileName)); !strings.Contains(string(b), "local") {
		t.Error("the config directory's own stitches.yaml was changed")
	}
}

func TestHealthListen(t *testing.T) {
	for listen, ok := range map[string]bool{":8081": true, "0.0.0.0:8081": true, "8081": false, ":0x1": false, ":70000": false} {
		_, err := Load(writeDir(t, map[string]string{ConfigFileName: "health:\n  listen: \"" + listen + "\"\n"}))
		if (err == nil) != ok {
			t.Errorf("health.listen %q: err = %v, want ok = %v", listen, err, ok)
		}
	}
}
