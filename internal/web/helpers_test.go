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
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/parser"
	gmutil "github.com/yuin/goldmark/util"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

func TestFlashFrom(t *testing.T) {
	for query, want := range map[string]string{
		"done=added&name=billing":   "Added stitch billing.",
		"done=removed&name=billing": "Removed stitch billing;",
		"done=hacked&name=billing":  "",
		"done=added&name=%3Cb%3E":   "", // not a stitch name, so not echoed back
		"":                          "",
	} {
		got := flashFrom(httptest.NewRequest("GET", "/stitches?"+query, nil))
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%q: flash = %q, want %q", query, got, want)
		}
	}
}

func TestParseStitchForm(t *testing.T) {
	req := func(v url.Values) *http.Request {
		r := httptest.NewRequest("POST", "/stitches", strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r
	}

	s, _, err := parseStitchForm(req(url.Values{
		"name": {" billing "}, "from_network": {"acme"}, "from_service": {" billing-api "},
		"to_network": {"partner"}, "to_service": {"billing"}, "precedence": {"required"},
		"cost": {"10"}, "connect_timeout": {"7s"}, "idle_timeout": {"1h"}, "app_data": {"annotate"},
	}), "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "billing" || s.From.Service != "billing-api" || s.From.Cost != 10 || s.From.Precedence != "required" ||
		s.To.ConnectTimeout != 7*time.Second || s.IdleTimeout != time.Hour || s.AppData != config.AppDataAnnotate || s.Disabled {
		t.Errorf("stitch = %+v", s)
	}

	// Editing keeps the stitch's name whatever the form says; defaults
	// fill what was left empty.
	s, _, err = parseStitchForm(req(url.Values{"name": {"renamed"}, "disabled": {"on"}}), "billing")
	if err != nil || s.Name != "billing" || !s.Disabled || s.To.ConnectTimeout != config.DefaultConnectTimeout {
		t.Errorf("edit: %+v, %v", s, err)
	}

	// Every bad field is reported, and what was typed comes back.
	_, f, err := parseStitchForm(req(url.Values{"cost": {"70000"}, "connect_timeout": {"soon"}, "idle_timeout": {"5"}}), "")
	for _, want := range []string{`cost "70000"`, `connect timeout "soon"`, `idle timeout "5"`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %s", err, want)
		}
	}
	if f.ConnectTimeout != "soon" || f.IdleTimeout != "5" {
		t.Errorf("form lost what was typed: %+v", f)
	}
}

func TestSweepExpiredTokens(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("valid", strconv.FormatInt(now.Add(time.Minute).Unix(), 10))
	write("expired", strconv.FormatInt(now.Add(-time.Minute).Unix(), 10))
	write("garbage", "not a time")

	sweepExpiredTokens(dir, now)
	for name, kept := range map[string]bool{"valid": true, "expired": false, "garbage": false} {
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != kept {
			t.Errorf("%s: kept = %v, want %v", name, err == nil, kept)
		}
	}
	sweepExpiredTokens(filepath.Join(dir, "missing"), now) // no directory: nothing to do
}

func TestDocLinks(t *testing.T) {
	md := goldmark.New(goldmark.WithParserOptions(parser.WithASTTransformers(
		gmutil.Prioritized(docLinks{published: map[string]bool{"install.md": true}}, 100))))
	var out bytes.Buffer
	src := "[a](install.md) [b](install.md#windows) [c](notes.md) [d](https://example.com/x.md) [e](../install.md)"
	if err := md.Convert([]byte(src), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		`<a href="/docs/install">a</a>`,
		`<a href="/docs/install#windows">b</a>`,
		` c `,                                      // an unpublished guide: the words stay, the link goes
		`<a href="https://example.com/x.md">d</a>`, // not a guide
		`<a href="../install.md">e</a>`,            // a path, not a guide next to this one
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered %s\nwant it to contain %s", got, want)
		}
	}
}
