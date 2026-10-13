//go:build e2e

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

// Runs against the local harness:
//
//	./deploy/local-e2e.sh up
//	make e2e-test

package web

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/events"
	"github.com/netfoundry/ziti-stitch/internal/network"
	"github.com/netfoundry/ziti-stitch/internal/runner"
)

// TestWebUIOverZiti serves the UI as the stitch-admin service on network
// A, bound by stitch-a through the runner, and browses it as client-a,
// dialing the service the way a tunneler would for a browser.
func TestWebUIOverZiti(t *testing.T) {
	harness := os.Getenv("ZITI_STITCH_E2E_DIR")
	if harness == "" {
		t.Skip("ZITI_STITCH_E2E_DIR not set; start ./deploy/local-e2e.sh up and run make e2e-test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	zClient, err := network.Connect(filepath.Join(harness, "a", "client-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer zClient.Close()
	me, err := zClient.GetCurrentIdentity()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := config.EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(harness, "a", "stitch-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "networks", "acme.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	setAccess := func(policy string, allow ...string) {
		t.Helper()
		cfg := "web:\n  ziti:\n    network: acme\n    service: stitch-admin\n    hosts: [stitch-admin.ziti]\n    policy_access: " + policy + "\n"
		if len(allow) > 0 {
			cfg += "    allow: [" + strings.Join(allow, ", ") + "]\n"
		}
		if err := os.WriteFile(filepath.Join(dir, config.ConfigFileName), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setAccess("none", *me.ID)

	d, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := runner.New(log)
	r.Apply(d)
	defer r.Shutdown(5 * time.Second)
	srv, err := New(Options{ConfigDir: dir, Web: d.Config.Web, Version: "e2e", Runner: r, Events: events.NewRecorder(100), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() { srv.Serve(ctx); close(served) }()
	defer func() { cancel(); <-served }()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return zClient.DialContext(ctx, "stitch-admin")
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := client.Get("http://stitch-admin.ziti" + path)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// The service takes a moment to bind.
	var code int
	var body string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if code, body = get("/stitches"); code == http.StatusOK {
			break
		}
	}
	if code != http.StatusOK || !strings.Contains(body, "client-a ("+*me.ID+")") || !strings.Contains(body, "Add a stitch") {
		t.Fatalf("allowlisted: %d\n%s", code, body)
	}

	setAccess("read")
	if code, body = get("/stitches"); code != http.StatusOK || strings.Contains(body, "Add a stitch") || !strings.Contains(body, "read-only") {
		t.Errorf("policy read: %d, add form shown: %v", code, strings.Contains(body, "Add a stitch"))
	}

	setAccess("none")
	if code, _ = get("/stitches"); code != http.StatusSeeOther {
		t.Errorf("no access: %d", code)
	}
	if code, body = get("/login"); code != http.StatusOK || !strings.Contains(body, *me.ID) {
		t.Errorf("login page over Ziti should name the refused identity: %d", code)
	}

	// The loopback-style Host is not this listener's.
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8474/login", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("foreign Host over Ziti: %d", resp.StatusCode)
	}
}
