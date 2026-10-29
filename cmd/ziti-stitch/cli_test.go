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
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/network"
)

// cli runs ziti-stitch with args against the config directory dir and
// returns stdout, stderr and the error.
func cli(t *testing.T, dir string, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"--config-dir", dir}, args...))
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func mustCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, errOut, err := cli(t, dir, args...)
	if err != nil {
		t.Fatalf("%v: %v\n%s%s", args, err, out, errOut)
	}
	return out
}

// identityFile writes an enrolled-looking identity for controller, with a
// certificate expiring at notAfter, and returns its path.
func identityFile(t *testing.T, controller string, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"}, NotBefore: notAfter.AddDate(-1, 0, 0), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{
		"ztAPI": controller + "/edge/client/v1",
		"id": map[string]string{
			"key":  "pem:KEY",
			"cert": "pem:" + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			"ca":   "pem:CA",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// twoNetworkDir returns a config directory with networks acme and partner.
func twoNetworkDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cfg")
	year := time.Now().AddDate(1, 0, 0)
	mustCLI(t, dir, "network", "add", "--identity", identityFile(t, "https://ctrl.acme.example.com:1280", year))
	mustCLI(t, dir, "network", "add", "--identity", identityFile(t, "https://partner.example.net", year))
	return dir
}

func TestNetworkAddDerivesNameAndRefusesDuplicates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	id := identityFile(t, "https://ctrl.acme.example.com:1280", time.Now().AddDate(1, 0, 0))

	out := mustCLI(t, dir, "network", "add", "--identity", id)
	if !strings.Contains(out, `Added network "acme"`) {
		t.Errorf("output = %q", out)
	}
	fi, err := os.Stat(filepath.Join(dir, "networks", "acme.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != network.IdentityFilePerm {
		t.Errorf("identity file mode = %v", fi.Mode().Perm())
	}

	if _, _, err := cli(t, dir, "network", "add", "--identity", id); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second add: err = %v", err)
	}
	_, errOut, err := cli(t, dir, "network", "add", "--identity", id, "--name", "acme2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, `network "acme" already uses controller ctrl.acme.example.com:1280`) {
		t.Errorf("no same-controller warning: %q", errOut)
	}
	if _, _, err := cli(t, dir, "network", "add", "--identity", id, "--name", "Bad Name"); err == nil {
		t.Error("an invalid name was accepted")
	}
}

// Only the identity file is copied, so it must hold its key inline.
func TestNetworkAddRefusesAKeyKeptElsewhere(t *testing.T) {
	path := identityFile(t, "https://ctrl.acme.example.com:1280", time.Now().AddDate(1, 0, 0))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"key":"pem:KEY"`), []byte(`"key":"file:///home/me/acme.key"`), 1)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "cfg")
	if _, _, err := cli(t, dir, "network", "add", "--identity", path); err == nil || !strings.Contains(err.Error(), "needs them inline") {
		t.Errorf("err = %v, want needs them inline", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "networks", "acme.json")); err == nil {
		t.Error("the identity was copied anyway")
	}
}

func TestNetworkAddNeedsANameForIPControllers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	id := identityFile(t, "https://10.1.2.3:1280", time.Now().AddDate(1, 0, 0))
	if _, _, err := cli(t, dir, "network", "add", "--identity", id); err == nil || !strings.Contains(err.Error(), "pass --name") {
		t.Errorf("err = %v", err)
	}
	mustCLI(t, dir, "network", "add", "--identity", id, "--name", "lab")
}

func TestStitchLifecycle(t *testing.T) {
	dir := twoNetworkDir(t)

	mustCLI(t, dir, "stitch", "add", "billing", "--from", "acme:billing-api", "--to", "partner:billing")
	mustCLI(t, dir, "stitch", "add", "odd", "--from", "acme:svc:with:colons", "--to", "partner:x", "--app-data", "annotate", "--idle-timeout", "1h", "--max-connections", "500", "--disabled")

	out := mustCLI(t, dir, "stitch", "list")
	for _, want := range []string{"billing", "acme:billing-api", "partner:billing", "passthrough", "enabled", "acme:svc:with:colons", "annotate", "disabled"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}

	raw, err := os.ReadFile(filepath.Join(dir, "stitches.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "passthrough") {
		t.Errorf("stitches.yaml spells out defaults:\n%s", raw)
	}
	if !strings.Contains(string(raw), "max_connections: 500") {
		t.Errorf("stitches.yaml lacks max_connections:\n%s", raw)
	}

	if _, _, err := cli(t, dir, "stitch", "add", "billing", "--from", "acme:a", "--to", "partner:b"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate add: err = %v", err)
	}
	if _, _, err := cli(t, dir, "stitch", "add", "lost", "--from", "nowhere:a", "--to", "partner:b"); err == nil || !strings.Contains(err.Error(), "not saved") {
		t.Errorf("add to an unknown network: err = %v", err)
	}
	if _, _, err := cli(t, dir, "stitch", "add", "loop", "--from", "partner:billing", "--to", "acme:billing-api"); err == nil || !strings.Contains(err.Error(), "loop") {
		t.Errorf("add making a loop: err = %v", err)
	}
	if _, _, err := cli(t, dir, "stitch", "add", "bad", "--from", "acme", "--to", "partner:b"); err == nil || !strings.Contains(err.Error(), "network:service") {
		t.Errorf("bad endpoint: err = %v", err)
	}

	mustCLI(t, dir, "stitch", "disable", "billing")
	mustCLI(t, dir, "stitch", "enable", "odd")
	out = mustCLI(t, dir, "stitch", "list")
	if !strings.Contains(lineWith(out, "billing "), "disabled") || !strings.Contains(lineWith(out, "odd "), "enabled") {
		t.Errorf("enable/disable not reflected:\n%s", out)
	}

	mustCLI(t, dir, "stitch", "remove", "odd")
	if out := mustCLI(t, dir, "stitch", "list"); strings.Contains(out, "odd") {
		t.Errorf("removed stitch still listed:\n%s", out)
	}
	if _, _, err := cli(t, dir, "stitch", "remove", "odd"); err == nil {
		t.Error("removing a missing stitch succeeded")
	}
}

func lineWith(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

func TestNetworkListRenameRemove(t *testing.T) {
	dir := twoNetworkDir(t)
	mustCLI(t, dir, "stitch", "add", "billing", "--from", "acme:a", "--to", "partner:b")

	out := mustCLI(t, dir, "network", "list")
	if !strings.Contains(lineWith(out, "acme "), "ctrl.acme.example.com:1280") || !strings.Contains(lineWith(out, "acme "), "billing") || !strings.Contains(out, "days)") {
		t.Errorf("list:\n%s", out)
	}

	if _, _, err := cli(t, dir, "network", "remove", "acme"); err == nil || !strings.Contains(err.Error(), "used by stitches billing") {
		t.Errorf("removing a used network: err = %v", err)
	}

	mustCLI(t, dir, "network", "rename", "acme", "corp")
	if _, err := os.Stat(filepath.Join(dir, "networks", "corp.json")); err != nil {
		t.Errorf("identity file not renamed: %v", err)
	}
	if out := mustCLI(t, dir, "stitch", "list"); !strings.Contains(out, "corp:a") {
		t.Errorf("stitch not updated by the rename:\n%s", out)
	}
	if _, _, err := cli(t, dir, "network", "rename", "corp", "partner"); err == nil {
		t.Error("renaming onto an existing network succeeded")
	}

	mustCLI(t, dir, "stitch", "remove", "billing")
	mustCLI(t, dir, "network", "remove", "corp")
	if _, err := os.Stat(filepath.Join(dir, "networks", "corp.json")); !os.IsNotExist(err) {
		t.Errorf("identity file still there: %v", err)
	}
}

func TestValidateOffline(t *testing.T) {
	dir := twoNetworkDir(t)
	mustCLI(t, dir, "stitch", "add", "billing", "--from", "acme:a", "--to", "partner:b")
	if out := mustCLI(t, dir, "validate", "--offline"); !strings.Contains(out, "is valid") {
		t.Errorf("validate: %q", out)
	}

	soon := identityFile(t, "https://soon.example.org", time.Now().Add(48*time.Hour))
	mustCLI(t, dir, "network", "add", "--identity", soon)
	mustCLI(t, dir, "stitch", "add", "expiring", "--from", "soon:x", "--to", "partner:y")
	out, _, err := cli(t, dir, "validate", "--offline")
	if err == nil || !strings.Contains(out, "problem  network soon: certificate expires") {
		t.Errorf("expiring certificate not flagged: err = %v\n%s", err, out)
	}
}

func TestCommandsNeedAConfigDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	for _, args := range [][]string{{"stitch", "list"}, {"network", "list"}, {"stitch", "remove", "s"}, {"network", "rename", "a", "b"}} {
		if _, _, err := cli(t, dir, args...); err == nil || !strings.Contains(err.Error(), "ziti-stitch enroll") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

func TestChangesWaitForTheEditLock(t *testing.T) {
	dir := twoNetworkDir(t)
	unlock, err := config.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := cli(t, dir, "stitch", "add", "billing", "--from", "acme:billing-api", "--to", "partner:billing")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("stitch add finished while another change held the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if out := mustCLI(t, dir, "stitch", "list"); !strings.Contains(out, "billing") {
		t.Errorf("stitch not saved:\n%s", out)
	}
}

func TestReadToken(t *testing.T) {
	if tok, err := readToken("-", strings.NewReader("  abc.def.ghi\n")); err != nil || tok != "abc.def.ghi" {
		t.Errorf("stdin token = %q, %v", tok, err)
	}
	if _, err := readToken("-", strings.NewReader("\n")); err == nil {
		t.Error("an empty token was accepted")
	}
}

func TestWebNetworkCannotBeRemovedOrRenamed(t *testing.T) {
	dir := twoNetworkDir(t)
	cfg := "web:\n  ziti:\n    network: acme\n    service: stitch-admin\n    hosts: [stitch-admin.ziti]\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"network", "remove", "acme"}, {"network", "rename", "acme", "corp"}} {
		if _, _, err := cli(t, dir, args...); err == nil || !strings.Contains(err.Error(), "web.ziti.network") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

func TestWebLoginPrintsALinkPerListener(t *testing.T) {
	dir := twoNetworkDir(t)
	if _, _, err := cli(t, dir, "web", "login"); err == nil || !strings.Contains(err.Error(), "web UI is off") {
		t.Errorf("with the UI off: err = %v", err)
	}
	cfg := "web:\n  listen: 127.0.0.1:8474\n  ziti:\n    network: acme\n    service: stitch-admin\n    hosts: [stitch-admin.ziti]\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out := mustCLI(t, dir, "web", "login")
	if !strings.Contains(out, "http://127.0.0.1:8474/login?t=") || !strings.Contains(out, "http://stitch-admin.ziti/login?t=") {
		t.Errorf("output:\n%s", out)
	}
}

// web.ziti without hosts is invalid, but web login does not validate.
func TestWebLoginWithoutZitiHosts(t *testing.T) {
	dir := twoNetworkDir(t)
	write := func(cfg string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("web:\n  ziti:\n    network: acme\n    service: stitch-admin\n")
	if _, _, err := cli(t, dir, "web", "login"); err == nil || !strings.Contains(err.Error(), "no hosts") {
		t.Errorf("Ziti only, no hosts: err = %v", err)
	}
	write("web:\n  listen: 127.0.0.1:8474\n  ziti:\n    network: acme\n    service: stitch-admin\n")
	if out := mustCLI(t, dir, "web", "login"); !strings.Contains(out, "http://127.0.0.1:8474/login?t=") || strings.Contains(out, "over Ziti") {
		t.Errorf("output:\n%s", out)
	}
}
