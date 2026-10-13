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
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
	path := filepath.Join(t.TempDir(), "acme.json")
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
