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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/diagnose"
	"github.com/netfoundry/ziti-stitch/internal/events"
	"github.com/netfoundry/ziti-stitch/internal/network"
	"github.com/netfoundry/ziti-stitch/internal/runner"
	"github.com/netfoundry/ziti-stitch/internal/stitch"
)

const testListen = "127.0.0.1:8474"

type fakeRunner struct{ snap runner.Snapshot }

// testLogWriter sends the server's log to the test's, so a failing test
// shows why (template errors, for instance).
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func (f fakeRunner) Snapshot() runner.Snapshot { return f.snap }

func (f fakeRunner) Network(string) (runner.Network, bool) { return nil, false }

// identityJSON is an enrolled-looking identity for controller.
func identityJSON(t *testing.T, controller string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"ztAPI": controller + "/edge/client/v1",
		"id":    map[string]string{"key": "pem:K", "cert": "pem:" + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))},
	})
	return b
}

type testEnv struct {
	t      *testing.T
	srv    *Server
	h      http.Handler
	dir    string
	events *events.Recorder
	cookie *http.Cookie
	csrf   string
	// via, when set, makes requests arrive over Ziti from this identity.
	via *caller
}

// newEnv returns a server for a config dir with networks acme and partner
// and one stitch, billing, with a snapshot showing it listening.
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	if err := config.EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	for name, ctrl := range map[string]string{"acme": "https://ctrl.acme.example.com", "partner": "https://partner.example.net"} {
		if err := os.WriteFile(filepath.Join(dir, "networks", name+".json"), identityJSON(t, ctrl), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d, err := config.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Edit(func(d *config.Dir) error {
		d.Stitches = append(d.Stitches, config.Stitch{Name: "billing", From: config.Ingress{Network: "acme", Service: "billing-api"}, To: config.Egress{Network: "partner", Service: "billing"}}.WithDefaults())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snap := runner.Snapshot{
		Networks:    []network.Status{{Name: "acme", Ready: true, IdentityName: "stitch-a"}, {Name: "partner", Ready: false, Err: "controller unreachable", ErrAt: time.Now()}},
		Stitches:    []stitch.Status{{Name: "billing", From: "acme:billing-api", To: "partner:billing", State: stitch.StateGated, Since: time.Now(), Active: 1, Accepted: 7, Failed: 2}},
		Connections: []stitch.ConnStatus{{Stitch: "billing", DialerIdentityName: "client-a", ACircuit: "circA", BCircuit: "circB", Opened: time.Now().Add(-time.Minute), BytesToEgress: 2048, BytesToIngress: 10}},
	}
	rec := events.NewRecorder(100)
	srv, err := New(Options{ConfigDir: dir, Web: config.Web{Listen: testListen, Ziti: &config.WebZiti{Network: "acme", Service: "stitch-admin", Hosts: []string{"stitch-admin.ziti"}}},
		Version: "v-test", Runner: fakeRunner{snap}, Events: rec, Log: slog.New(rec.Handler(slog.NewTextHandler(testLogWriter{t}, nil), slog.LevelInfo))})
	if err != nil {
		t.Fatal(err)
	}
	srv.probe = func(name, _ string) diagnose.NetworkReport {
		if name == "partner" {
			return diagnose.NetworkReport{Name: name, Err: errors.New("connection refused")}
		}
		return diagnose.NetworkReport{Name: name, IdentityName: "stitch-a", Services: map[string]diagnose.Access{"billing-api": {Dial: true}}}
	}
	return &testEnv{t: t, srv: srv, h: srv.Handler(), dir: dir, events: rec}
}

func (e *testEnv) do(req *http.Request) *httptest.ResponseRecorder {
	if req.Host == "example.com" { // httptest's default
		req.Host = testListen
		if e.via != nil {
			req.Host = "stitch-admin.ziti"
		}
	}
	if e.via != nil {
		// What withCaller does for a connection from a Ziti listener.
		req = req.WithContext(context.WithValue(req.Context(), callerKey{}, *e.via))
	}
	if e.cookie != nil {
		req.AddCookie(e.cookie)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	// Keep a session the server hands out, as a browser would.
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge > 0 {
			e.cookie = c
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.AddCookie(c)
			if sess, ok := e.srv.sessionFrom(r); ok {
				e.csrf = sess.CSRF
			}
		}
	}
	return w
}

func (e *testEnv) get(path string) *httptest.ResponseRecorder {
	return e.do(httptest.NewRequest(http.MethodGet, path, nil))
}

// post sends form values, with the session's CSRF token unless the form
// sets "csrf" itself.
func (e *testEnv) post(path string, form url.Values) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	if _, set := form["csrf"]; !set {
		form.Set("csrf", e.csrf)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return e.do(req)
}

// signIn redeems a login link; do keeps the session it hands out.
func (e *testEnv) signIn() {
	e.t.Helper()
	token, err := NewLoginToken(e.dir)
	if err != nil {
		e.t.Fatal(err)
	}
	w := e.get("/login?t=" + token)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" || e.csrf == "" {
		e.t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
}

// setZitiAccess rewrites config.yaml's web.ziti allowlist and policy.
func (e *testEnv) setZitiAccess(policy string, allow ...string) {
	e.t.Helper()
	cfg := "web:\n  listen: " + testListen + "\n  ziti:\n    network: acme\n    service: stitch-admin\n    hosts: [stitch-admin.ziti]\n    policy_access: " + policy + "\n"
	if len(allow) > 0 {
		cfg += "    allow: [" + strings.Join(allow, ", ") + "]\n"
	}
	if err := os.WriteFile(filepath.Join(e.dir, config.ConfigFileName), []byte(cfg), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) stitches() []config.Stitch {
	e.t.Helper()
	d, err := config.Read(e.dir)
	if err != nil {
		e.t.Fatal(err)
	}
	return d.Stitches
}

func body(w *httptest.ResponseRecorder) string { return w.Body.String() }

func TestForeignHostIsRefused(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.Host = "evil.example:8474"
	if w := e.do(req); w.Code != http.StatusMisdirectedRequest {
		t.Errorf("foreign Host: %d", w.Code)
	}
	for _, h := range []string{"localhost:8474", "[::1]:8474", "127.0.0.1:8474"} {
		req := httptest.NewRequest(http.MethodGet, "/login", nil)
		req.Host = h
		if w := e.do(req); w.Code != http.StatusOK {
			t.Errorf("Host %s: %d", h, w.Code)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	w := e.get("/login")
	h := w.Header()
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP = %q", csp)
	}
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", h)
	}
	if w := e.get("/static/style.css"); w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Cache-Control"), "max-age") {
		t.Errorf("static: %d %v", w.Code, w.Header())
	}
}

func TestSignedOutRequestsGoToLogin(t *testing.T) {
	e := newEnv(t)
	if w := e.get("/stitches"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Errorf("GET: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := e.post("/stitches/billing/disable", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("POST: %d", w.Code)
	}
	if w := e.get("/login"); w.Code != http.StatusOK || !strings.Contains(body(w), "ziti-stitch web login") {
		t.Errorf("login page: %d", w.Code)
	}
}

func TestLoginLinkWorksOnce(t *testing.T) {
	e := newEnv(t)
	token, err := NewLoginToken(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	if w := e.get("/login?t=" + token); w.Code != http.StatusSeeOther {
		t.Fatalf("first use: %d", w.Code)
	}
	if w := e.get("/login?t=" + token); w.Code != http.StatusUnauthorized || !strings.Contains(body(w), "not valid") {
		t.Errorf("second use: %d", w.Code)
	}
	if w := e.get("/login?t=made-up"); w.Code != http.StatusUnauthorized {
		t.Errorf("made-up token: %d", w.Code)
	}
}

func TestExpiredLoginLinkIsRefusedAndSwept(t *testing.T) {
	e := newEnv(t)
	token, err := NewLoginToken(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.now = func() time.Time { return time.Now().Add(loginLifetime + time.Second) }
	if w := e.get("/login?t=" + token); w.Code != http.StatusUnauthorized {
		t.Errorf("expired token: %d", w.Code)
	}
	entries, _ := os.ReadDir(filepath.Join(e.dir, webDirName, loginDirName))
	if len(entries) != 0 {
		t.Errorf("%d token files left", len(entries))
	}
}

func TestSessionCannotBeForgedOrOutlived(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	payload, _, _ := strings.Cut(e.cookie.Value, ".")
	e.cookie = &http.Cookie{Name: sessionCookie, Value: payload + ".forged"}
	if w := e.get("/"); w.Code != http.StatusSeeOther {
		t.Errorf("forged cookie: %d", w.Code)
	}

	e.signIn()
	e.srv.now = func() time.Time { return time.Now().Add(sessionLifetime + time.Minute) }
	if w := e.get("/"); w.Code != http.StatusSeeOther {
		t.Errorf("expired session: %d", w.Code)
	}
}

func TestPostNeedsCSRFToken(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	if w := e.post("/stitches/billing/disable", url.Values{"csrf": {"wrong"}}); w.Code != http.StatusForbidden {
		t.Errorf("wrong token: %d", w.Code)
	}
	if e.stitches()[0].Disabled {
		t.Fatal("a POST without the right token changed the config")
	}
	if w := e.post("/stitches/billing/disable", nil); w.Code != http.StatusSeeOther {
		t.Errorf("right token: %d %s", w.Code, body(w))
	}
}

func TestOverview(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	w := e.get("/")
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, body(w))
	}
	for _, want := range []string{"billing", "acme:billing-api", "gated", "stitch-a", "controller unreachable", "of 2</span></div><div class=\"l\">networks ready", "v-test"} {
		if !strings.Contains(body(w), want) {
			t.Errorf("overview lacks %q", want)
		}
	}
}

func TestStitchAddEditToggleRemove(t *testing.T) {
	e := newEnv(t)
	e.signIn()

	add := url.Values{"name": {"orders"}, "from_network": {"acme"}, "from_service": {"orders-api"}, "to_network": {"partner"}, "to_service": {"orders"},
		"precedence": {"required"}, "cost": {"5"}, "connect_timeout": {"3s"}, "app_data": {"annotate"}, "idle_timeout": {"1h"}}
	w := e.post("/stitches", add)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/stitches?done=added&name=orders" {
		t.Fatalf("add: %d %s", w.Code, body(w))
	}
	got := e.stitches()[1]
	want := config.Stitch{Name: "orders", From: config.Ingress{Network: "acme", Service: "orders-api", Precedence: "required", Cost: 5},
		To: config.Egress{Network: "partner", Service: "orders", ConnectTimeout: 3 * time.Second}, AppData: config.AppDataAnnotate, IdleTimeout: time.Hour}
	if got != want {
		t.Errorf("saved %+v\nwant  %+v", got, want)
	}
	if w := e.get("/stitches?done=added&name=orders"); !strings.Contains(body(w), "Added stitch orders.") {
		t.Error("no flash after adding")
	}

	edit := url.Values{"from_network": {"acme"}, "from_service": {"orders-api"}, "to_network": {"partner"}, "to_service": {"orders-v2"}, "precedence": {"default"}, "app_data": {"passthrough"}, "connect_timeout": {"5s"}}
	if w := e.post("/stitches/orders", edit); w.Code != http.StatusSeeOther {
		t.Fatalf("edit: %d %s", w.Code, body(w))
	}
	if got := e.stitches()[1]; got.To.Service != "orders-v2" || got.From.Cost != 0 || got.IdleTimeout != 0 {
		t.Errorf("after edit: %+v", got)
	}
	if w := e.get("/stitches/orders"); w.Code != http.StatusOK || !strings.Contains(body(w), `value="orders-v2"`) {
		t.Errorf("edit page: %d", w.Code)
	}

	if w := e.post("/stitches/orders/disable", nil); w.Code != http.StatusSeeOther || !e.stitches()[1].Disabled {
		t.Errorf("disable: %d", w.Code)
	}
	if w := e.post("/stitches/orders/enable", nil); w.Code != http.StatusSeeOther || e.stitches()[1].Disabled {
		t.Errorf("enable: %d", w.Code)
	}
	if w := e.post("/stitches/orders/remove", nil); w.Code != http.StatusSeeOther || len(e.stitches()) != 1 {
		t.Errorf("remove: %d", w.Code)
	}
	if w := e.post("/stitches/orders/remove", nil); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("removing it again: %d", w.Code)
	}
}

func TestInvalidStitchIsNotSavedAndTheFormComesBack(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	tests := map[string]struct {
		form url.Values
		want string
	}{
		"duplicate":    {url.Values{"name": {"billing"}, "from_network": {"acme"}, "from_service": {"x"}, "to_network": {"partner"}, "to_service": {"y"}}, "already exists"},
		"loop":         {url.Values{"name": {"back"}, "from_network": {"partner"}, "from_service": {"billing"}, "to_network": {"acme"}, "to_service": {"billing-api"}}, "loop"},
		"bad duration": {url.Values{"name": {"x"}, "from_network": {"acme"}, "from_service": {"x"}, "to_network": {"partner"}, "to_service": {"y"}, "connect_timeout": {"soon"}}, "not a duration"},
		"bad name":     {url.Values{"name": {"Bad Name"}, "from_network": {"acme"}, "from_service": {"x"}, "to_network": {"partner"}, "to_service": {"y"}}, "name is not valid"},
	}
	for name, tt := range tests {
		w := e.post("/stitches", tt.form)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(body(w), tt.want) {
			t.Errorf("%s: %d, body lacks %q", name, w.Code, tt.want)
		}
		if n := len(e.stitches()); n != 1 {
			t.Fatalf("%s: saved anyway (%d stitches)", name, n)
		}
	}
	// What was typed comes back for fixing.
	w := e.post("/stitches", url.Values{"name": {"x"}, "from_network": {"acme"}, "from_service": {"keep-me"}, "to_network": {"partner"}, "to_service": {"y"}, "connect_timeout": {"soon"}})
	if !strings.Contains(body(w), `value="keep-me"`) || !strings.Contains(body(w), `value="soon"`) {
		t.Error("the form did not keep what was typed")
	}
}

func TestFlashIsOnlyFromKnownCodesAndNames(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	for _, q := range []string{"done=hacked&name=billing", "done=added&name=" + url.QueryEscape("<b>you are compromised</b>")} {
		if b := body(e.get("/stitches?" + q)); strings.Contains(b, "class=\"flash ok\"") {
			t.Errorf("flash shown for %s", q)
		}
	}
}

func TestNetworkTest(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	req := httptest.NewRequest(http.MethodPost, "/networks/acme/test", strings.NewReader(url.Values{"csrf": {e.csrf}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	w := e.do(req)
	b := body(w)
	if w.Code != http.StatusOK || strings.Contains(b, "<html") {
		t.Fatalf("partial: %d, full page = %v", w.Code, strings.Contains(b, "<html"))
	}
	if !strings.Contains(b, "billing-api") || !strings.Contains(b, "can dial acme:billing-api but not bind it") {
		t.Errorf("test result:\n%s", b)
	}

	// Without htmx the whole page comes back, result included.
	w = e.post("/networks/partner/test", nil)
	if !strings.Contains(body(w), "<html") || !strings.Contains(body(w), "Cannot authenticate: connection refused") {
		t.Errorf("full page result: %d", w.Code)
	}
	if w := e.post("/networks/nope/test", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown network: %d", w.Code)
	}
}

func TestConnectionsPageAndPartial(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	w := e.get("/connections")
	if !strings.Contains(body(w), "client-a") || !strings.Contains(body(w), "2.0 KiB") || !strings.Contains(body(w), `hx-trigger="every 2s"`) {
		t.Errorf("connections page:\n%s", body(w))
	}
	req := httptest.NewRequest(http.MethodGet, "/connections", nil)
	req.Header.Set("HX-Request", "true")
	if b := body(e.do(req)); strings.Contains(b, "<html") || !strings.Contains(b, "circA") {
		t.Error("the htmx refresh should get the table only")
	}
}

func TestEventsFilter(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	log := slog.New(e.events.Handler(slog.NewTextHandler(io.Discard, nil), slog.LevelInfo))
	log.Info("connection opened", "stitch", "billing", "a_circuit", "c1")
	log.Warn("cannot bind", "stitch", "orders")
	log.Error("renewal failed", "network", "acme")

	if b := body(e.get("/events")); !strings.Contains(b, "connection opened") || !strings.Contains(b, "renewal failed") || !strings.Contains(b, "c1") {
		t.Errorf("all events:\n%s", b)
	}
	b := body(e.get("/events?stitch=billing"))
	if !strings.Contains(b, "connection opened") || strings.Contains(b, "cannot bind") {
		t.Error("stitch filter")
	}
	b = body(e.get("/events?level=warn"))
	if strings.Contains(b, "connection opened") || !strings.Contains(b, "cannot bind") || !strings.Contains(b, "renewal failed") {
		t.Error("level filter")
	}
}

func TestDocs(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	if b := body(e.get("/docs/")); !strings.Contains(b, `href="/docs/install"`) || !strings.Contains(b, "What to ask your network admins for") {
		t.Errorf("index:\n%s", b)
	}
	w := e.get("/docs/install")
	if w.Code != http.StatusOK || !strings.Contains(body(w), `href="/docs/admin-checklist"`) {
		t.Errorf("install guide: %d (links between guides should be rewritten)", w.Code)
	}
	if w := e.get("/docs/nope"); w.Code != http.StatusNotFound {
		t.Errorf("missing guide: %d", w.Code)
	}
}

func TestUnknownPage(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	if w := e.get("/nope"); w.Code != http.StatusNotFound {
		t.Errorf("%d", w.Code)
	}
}

func TestSignOut(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	w := e.post("/logout", nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("%d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge >= 0 {
			t.Error("session cookie not cleared")
		}
	}
}

func TestSessionKeyPersists(t *testing.T) {
	dir := t.TempDir()
	k1, err := loadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := loadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(k1) != string(k2) || len(k1) != 32 {
		t.Error("the key changed between loads")
	}
	if fi, err := os.Stat(filepath.Join(dir, webDirName, keyFileName)); err != nil || fi.Mode().Perm() != privateFilePerm {
		t.Errorf("key file: %v %v", fi, err)
	}
}

func TestHumanHelpers(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 20: "5.0 MiB", 3 << 30: "3.0 GiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %s, want %s", n, got, want)
		}
	}
	for d, want := range map[time.Duration]string{45 * time.Second: "45s", 12 * time.Minute: "12m", 3*time.Hour + 5*time.Minute: "3h 5m", 50 * time.Hour: "2d 2h"} {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %s, want %s", d, got, want)
		}
	}
	if ago(time.Time{}) != "" || ago(time.Now()) != "just now" {
		t.Error("ago")
	}
}

var alice = &caller{ID: "AliceID", Name: "alice"}

func TestZitiAllowlistedIdentityGetsFullAccess(t *testing.T) {
	e := newEnv(t)
	e.setZitiAccess("none", "AliceID")
	e.via = alice

	w := e.get("/stitches")
	if w.Code != http.StatusOK || e.csrf == "" {
		t.Fatalf("GET: %d, session issued: %v", w.Code, e.csrf != "")
	}
	if b := body(w); !strings.Contains(b, "Add a stitch") || !strings.Contains(b, "alice (AliceID)") || strings.Contains(b, "Sign out") {
		t.Error("an allowlisted identity should see the edit controls and who it is, and no sign-out")
	}
	if w := e.post("/stitches/billing/disable", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("POST: %d %s", w.Code, body(w))
	}
	if !e.stitches()[0].Disabled {
		t.Error("the edit was not saved")
	}
	if ev := e.events.Events()[0]; ev.Message != "stitch disabled in the web UI" || ev.Attr("by") != "alice (AliceID)" {
		t.Errorf("audit event = %q by %q", ev.Message, ev.Attr("by"))
	}
}

func TestZitiIdentityWithoutAccessIsShownWhy(t *testing.T) {
	e := newEnv(t)
	e.setZitiAccess("none", "SomeoneElse")
	e.via = alice

	if w := e.get("/"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Errorf("GET /: %d %s", w.Code, w.Header().Get("Location"))
	}
	b := body(e.get("/login"))
	if !strings.Contains(b, "<strong>alice</strong> (<code>AliceID</code>)") || !strings.Contains(b, "web.ziti.allow") {
		t.Errorf("login page does not say which identity was refused:\n%s", b)
	}
	if w := e.post("/stitches/billing/disable", url.Values{"csrf": {""}}); w.Code != http.StatusUnauthorized {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestZitiPolicyReadIsReadOnly(t *testing.T) {
	e := newEnv(t)
	e.setZitiAccess("read")
	e.via = alice

	w := e.get("/stitches")
	b := body(w)
	if w.Code != http.StatusOK || strings.Contains(b, "Add a stitch") || !strings.Contains(b, "read-only") {
		t.Errorf("stitches page for read-only: %d, add form shown: %v", w.Code, strings.Contains(b, "Add a stitch"))
	}
	if b := body(e.get("/stitches/billing")); !strings.Contains(b, "<fieldset class=\"bare\" disabled>") || strings.Contains(b, ">Remove</button>") {
		t.Error("the edit page should show the settings disabled, and no actions")
	}
	if w := e.post("/stitches/billing/disable", nil); w.Code != http.StatusForbidden {
		t.Errorf("edit with read access: %d", w.Code)
	}
	if e.stitches()[0].Disabled {
		t.Fatal("a read-only identity changed the config")
	}
	if w := e.post("/networks/acme/test", nil); w.Code != http.StatusOK {
		t.Errorf("testing a network needs only read access: %d", w.Code)
	}
}

func TestZitiPolicyEditTrustsThePolicy(t *testing.T) {
	e := newEnv(t)
	e.setZitiAccess("edit")
	e.via = alice
	e.get("/")
	if w := e.post("/stitches/billing/disable", nil); w.Code != http.StatusSeeOther {
		t.Errorf("POST with policy_access edit: %d", w.Code)
	}
}

func TestZitiAccessChangesApplyAtOnce(t *testing.T) {
	e := newEnv(t)
	e.setZitiAccess("read")
	e.via = alice
	e.get("/")
	if w := e.post("/stitches/billing/disable", nil); w.Code != http.StatusForbidden {
		t.Fatalf("before: %d", w.Code)
	}
	e.setZitiAccess("read", "AliceID")
	if w := e.post("/stitches/billing/disable", nil); w.Code != http.StatusSeeOther {
		t.Errorf("after allowlisting: %d", w.Code)
	}
	e.setZitiAccess("none")
	if w := e.get("/"); w.Code != http.StatusSeeOther {
		t.Errorf("after revoking, the session still works: %d", w.Code)
	}
}

func TestZitiAccessSurvivesAnUnreadableConfig(t *testing.T) {
	e := newEnv(t)
	e.setZitiAccess("none", "AliceID")
	e.via = alice
	if w := e.get("/"); w.Code != http.StatusOK {
		t.Fatalf("%d", w.Code)
	}
	if err := os.WriteFile(filepath.Join(e.dir, config.ConfigFileName), []byte("web: [this is: not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The connections page needs only the running state, not the config.
	if w := e.get("/connections"); w.Code != http.StatusOK {
		t.Errorf("a config that does not parse locked the allowlisted identity out: %d", w.Code)
	}
}

func TestZitiSessionIsBoundToItsIdentity(t *testing.T) {
	e := newEnv(t)
	e.setZitiAccess("none", "AliceID", "BobID")
	e.via = alice
	e.get("/")
	aliceCSRF := e.csrf

	// Bob's browser presenting Alice's cookie: a GET gets Bob his own
	// session, and Alice's token does not work for him.
	e.via = &caller{ID: "BobID", Name: "bob"}
	if w := e.post("/stitches/billing/disable", url.Values{"csrf": {aliceCSRF}}); w.Code != http.StatusForbidden {
		t.Errorf("Alice's session used by Bob: %d", w.Code)
	}
	e.get("/")
	if e.csrf == aliceCSRF {
		t.Error("Bob was not given his own session")
	}
}

func TestZitiHostCheck(t *testing.T) {
	e := newEnv(t)
	e.setZitiAccess("read")
	e.via = alice
	for host, want := range map[string]int{
		"stitch-admin.ziti":    http.StatusOK,
		"Stitch-Admin.Ziti:80": http.StatusOK,
		"evil.example":         http.StatusMisdirectedRequest,
		testListen:             http.StatusMisdirectedRequest, // the loopback address is not this listener's
	} {
		req := httptest.NewRequest(http.MethodGet, "/connections", nil)
		req.Host = host
		if w := e.do(req); w.Code != want {
			t.Errorf("Host %s over Ziti: %d, want %d", host, w.Code, want)
		}
	}
}

func TestLoginLinkWorksOverZiti(t *testing.T) {
	e := newEnv(t)
	e.setZitiAccess("none")
	e.via = alice
	e.signIn()
	if w := e.post("/stitches/billing/disable", nil); w.Code != http.StatusSeeOther {
		t.Errorf("a login link's session over Ziti: %d", w.Code)
	}
}

func TestPostWithoutSessionIsRefusedEvenWithEmptyToken(t *testing.T) {
	e := newEnv(t)
	e.setZitiAccess("none", "AliceID")
	e.via = alice
	// No GET first, so no session: an empty token must not match the
	// empty token of the missing session.
	if w := e.post("/stitches/billing/disable", url.Values{"csrf": {""}}); w.Code != http.StatusForbidden {
		t.Errorf("%d", w.Code)
	}
	if e.stitches()[0].Disabled {
		t.Fatal("changed without a CSRF token")
	}
}
