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

package network

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apis "github.com/openziti/sdk-golang/edge-apis"
	"github.com/openziti/sdk-golang/ziti"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

const sampleIdentity = `{
  "ztAPI": "https://ctrl.example.com:1280/edge/client/v1",
  "ztAPIs": null,
  "id": {
    "key": "pem:OLD KEY",
    "cert": "pem:OLD CERT",
    "ca": "pem:OLD CA"
  },
  "configTypes": null,
  "enableHa": true
}`

func TestRenewalDue(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	year := start.AddDate(1, 0, 0)
	week := start.Add(7 * 24 * time.Hour)
	tests := []struct {
		name                string
		notBefore, notAfter time.Time
		now                 time.Time
		want                bool
	}{
		{"year cert, fresh", start, year, start.Add(time.Hour), false},
		{"year cert, 31 days left", start, year, year.Add(-31 * 24 * time.Hour), false},
		{"year cert, 30 days left", start, year, year.Add(-30 * 24 * time.Hour), true},
		{"year cert, expired", start, year, year.Add(time.Hour), true},
		{"week cert, two thirds used", start, week, week.Add(-week.Sub(start) / 3).Add(-time.Minute), false},
		{"week cert, last third", start, week, week.Add(-week.Sub(start) / 3), true},
	}
	for _, tt := range tests {
		if got := renewalDue(tt.notBefore, tt.notAfter, tt.now); got != tt.want {
			t.Errorf("%s: renewalDue = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSetControllerURLs(t *testing.T) {
	urls := []string{"https://a:1280/edge/client/v1", "https://b:1280/edge/client/v1"}
	out, changed, err := setControllerURLs([]byte(sampleIdentity), urls)
	if err != nil || !changed {
		t.Fatalf("setControllerURLs = changed %v, err %v", changed, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc["ztAPIs"].([]any); len(got) != 2 || got[0] != urls[0] || got[1] != urls[1] {
		t.Errorf("ztAPIs = %v", got)
	}
	if doc["ztAPI"] != "https://ctrl.example.com:1280/edge/client/v1" || doc["enableHa"] != true {
		t.Errorf("other fields changed: %v", doc)
	}
	if id := doc["id"].(map[string]any); id["key"] != "pem:OLD KEY" {
		t.Errorf("id changed: %v", id)
	}

	again, changed, err := setControllerURLs(out, urls)
	if err != nil || changed || string(again) != string(out) {
		t.Errorf("same list again: changed %v, err %v, content changed %v", changed, err, string(again) != string(out))
	}
}

func TestSetControllerURLsRejectsBadJSON(t *testing.T) {
	if _, _, err := setControllerURLs([]byte("{"), nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestReplaceCredentials(t *testing.T) {
	out, err := replaceCredentials([]byte(sampleIdentity), "NEW KEY", "NEW CERT", "NEW CA")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := credentials(out)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Key != "pem:NEW KEY" || cfg.Cert != "pem:NEW CERT" || cfg.CA != "pem:NEW CA" {
		t.Errorf("credentials = %+v", cfg)
	}
	if !strings.Contains(string(out), `"enableHa": true`) {
		t.Errorf("other fields lost:\n%s", out)
	}
}

func TestReplaceCredentialsKeepsCAWhenNoneGiven(t *testing.T) {
	out, err := replaceCredentials([]byte(sampleIdentity), "NEW KEY", "NEW CERT", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := credentials(out)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CA != "pem:OLD CA" {
		t.Errorf("CA = %q, want the old one", cfg.CA)
	}
}

func TestReplaceCredentialsNeedsIDObject(t *testing.T) {
	if _, err := replaceCredentials([]byte(`{"ztAPI":"x"}`), "k", "c", ""); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCredentialsNeedsKeyAndCert(t *testing.T) {
	for _, raw := range []string{`{}`, `{"id":{"key":"pem:k"}}`, `{"id":{"cert":"pem:c"}}`, `not json`} {
		if _, err := credentials([]byte(raw)); err == nil {
			t.Errorf("credentials(%s): expected an error", raw)
		}
	}
}

func testNetwork(t *testing.T) *Network {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "networks")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "acme.json")
	if err := os.WriteFile(path, []byte(sampleIdentity), 0o600); err != nil {
		t.Fatal(err)
	}
	return New("acme", path, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestSettlePendingPromotesWorkingPendingFile(t *testing.T) {
	n := testNetwork(t)
	if err := os.WriteFile(n.pendingPath(), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := n.settlePending(n.pendingPath(), n.pendingPath()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(n.path)
	if string(got) != "new" {
		t.Errorf("identity file = %q, want the pending content", got)
	}
	if _, err := os.Stat(n.pendingPath()); !os.IsNotExist(err) {
		t.Errorf("pending file still exists")
	}
}

func TestSettlePendingDiscardsUnconfirmedRenewal(t *testing.T) {
	n := testNetwork(t)
	if err := os.WriteFile(n.pendingPath(), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := n.settlePending(n.path, n.pendingPath()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(n.path)
	if string(got) != sampleIdentity {
		t.Errorf("identity file changed")
	}
	if _, err := os.Stat(n.pendingPath()); !os.IsNotExist(err) {
		t.Errorf("pending file still exists")
	}
}

func TestSettlePendingWithoutPendingFile(t *testing.T) {
	n := testNetwork(t)
	if err := n.settlePending(n.path, n.pendingPath()); err != nil {
		t.Fatal(err)
	}
}

func TestSaveControllerURLs(t *testing.T) {
	n := testNetwork(t)
	u, _ := url.Parse("https://b:1280/edge/client/v1")
	n.saveControllerURLs(n.urlsSeq.Add(1), []*url.URL{u})

	raw, _ := os.ReadFile(n.path)
	if !strings.Contains(string(raw), "https://b:1280/edge/client/v1") {
		t.Fatalf("list not saved:\n%s", raw)
	}
	if fi, _ := os.Stat(n.path); runtime.GOOS != "windows" && fi.Mode().Perm() != IdentityFilePerm {
		t.Errorf("perm = %o, want %o", fi.Mode().Perm(), IdentityFilePerm)
	}
}

// A renewal waiting to be settled will replace the identity file with the
// pending one, so a new controller list must reach both.
func TestSaveControllerURLsUpdatesAPendingRenewal(t *testing.T) {
	n := testNetwork(t)
	raw, err := os.ReadFile(n.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.pendingPath(), raw, IdentityFilePerm); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://moved:1280/edge/client/v1")
	n.saveControllerURLs(n.urlsSeq.Add(1), []*url.URL{u})
	for _, p := range []string{n.path, n.pendingPath()} {
		if b, _ := os.ReadFile(p); !strings.Contains(string(b), "https://moved:1280") {
			t.Errorf("%s lacks the new controller list", filepath.Base(p))
		}
	}
}

// Identity files are written under the config directory's edit lock, and
// not at all once the CLI has renamed or removed them.
func TestWhileIdentityExists(t *testing.T) {
	n := testNetwork(t)
	unlock, err := config.Lock(filepath.Dir(filepath.Dir(n.path)))
	if err != nil {
		t.Fatal(err)
	}
	var wrote atomic.Bool
	done := make(chan error, 1)
	go func() { done <- n.whileIdentityExists(func() error { wrote.Store(true); return nil }) }()
	time.Sleep(200 * time.Millisecond)
	if wrote.Load() {
		t.Fatal("wrote while the CLI held the lock")
	}
	unlock()
	if err := <-done; err != nil || !wrote.Load() {
		t.Fatalf("after the lock was released: err %v, wrote %v", err, wrote.Load())
	}

	if err := os.Remove(n.path); err != nil {
		t.Fatal(err)
	}
	wrote.Store(false)
	if err := n.whileIdentityExists(func() error { wrote.Store(true); return nil }); err == nil || wrote.Load() {
		t.Errorf("identity file removed: err %v, wrote %v", err, wrote.Load())
	}
	u, _ := url.Parse("https://b:1280/edge/client/v1")
	n.saveControllerURLs(n.urlsSeq.Add(1), []*url.URL{u})
	if _, err := os.Stat(n.path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a controller list save brought the removed identity file back: %v", err)
	}
}

func TestSaveControllerURLsSkipsStaleUpdate(t *testing.T) {
	n := testNetwork(t)
	stale := n.urlsSeq.Add(1)
	n.urlsSeq.Add(1) // a newer update arrived
	u, _ := url.Parse("https://stale:1280/edge/client/v1")
	n.saveControllerURLs(stale, []*url.URL{u})

	raw, _ := os.ReadFile(n.path)
	if strings.Contains(string(raw), "stale") {
		t.Fatal("stale list was saved")
	}
}

func TestContextWaitsForReady(t *testing.T) {
	n := testNetwork(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := n.Context(ctx); err == nil {
		t.Fatal("Context returned before the network was ready")
	}
}

// authEvents delivers the SDK's authentication events on demand.
type authEvents struct {
	ziti.Eventer
	lost, back func(ziti.Context, apis.ApiSession)
}

func (e *authEvents) AddAuthenticationStateUnauthenticatedListener(f func(ziti.Context, apis.ApiSession)) func() {
	e.lost = f
	return func() { e.lost = nil }
}

func (e *authEvents) AddAuthenticationStateFullListener(f func(ziti.Context, apis.ApiSession)) func() {
	e.back = f
	return func() { e.back = nil }
}

func TestReadyFollowsTheAPISession(t *testing.T) {
	n := testNetwork(t)
	n.updateStatus(func(s *Status) { s.Ready = true })
	ev := &authEvents{}
	stop := n.followAuthentication(ev)

	ev.lost(nil, nil)
	if n.Status().Ready {
		t.Error("still ready after losing the API session")
	}
	ev.back(nil, nil)
	if !n.Status().Ready {
		t.Error("not ready after getting a new API session")
	}
	stop()
	if ev.lost != nil || ev.back != nil {
		t.Error("still following after stop")
	}
}

func TestStatusBeforeRun(t *testing.T) {
	n := testNetwork(t)
	if st := n.Status(); st.Name != "acme" || st.Ready || st.Err != "" {
		t.Errorf("status = %+v", st)
	}
	n.problem(errors.New("controller unreachable"))
	if st := n.Status(); st.Err != "controller unreachable" || st.ErrAt.IsZero() {
		t.Errorf("status after a problem = %+v", st)
	}
}
