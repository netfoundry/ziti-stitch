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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

func TestEnrollFrom(t *testing.T) {
	dir := twoNetworkDir(t) // acme and partner
	src := t.TempDir()
	put := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(src, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	other, err := os.ReadFile(identityFile(t, "https://ctrl.other.example.com", time.Now().AddDate(1, 0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	put("other.json", other)                  // new: copied in
	put("acme.jwt", []byte("not even a jwt")) // already there: never redeemed
	put("README", []byte("ignored"))
	if err := os.Mkdir(filepath.Join(src, "..data"), 0o700); err != nil { // a Secret volume's own entry
		t.Fatal(err)
	}

	d, err := config.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	acmeBefore, _ := os.ReadFile(d.IdentityPath("acme"))
	var out bytes.Buffer
	if err := enrollFrom(src, "", d, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `Added network "other"`) {
		t.Errorf("output = %q", out.String())
	}
	if b, err := os.ReadFile(d.IdentityPath("other")); err != nil || !bytes.Equal(b, other) {
		t.Errorf("other.json was not copied in: %v", err)
	}
	if b, _ := os.ReadFile(d.IdentityPath("acme")); !bytes.Equal(b, acmeBefore) {
		t.Error("an existing network's identity was replaced")
	}

	// Run again, as on every start: nothing more to add, but other.json
	// holds a private key that is not needed any more.
	d, _ = config.Read(dir)
	out.Reset()
	if err := enrollFrom(src, "", d, &out); err != nil || strings.Contains(out.String(), "Added") || !strings.Contains(out.String(), "does not use (other.json)") {
		t.Errorf("second run: %q, %v", out.String(), err)
	}

	// A token that cannot be redeemed stops startup.
	put("newnet.jwt", []byte("not a jwt"))
	if err := enrollFrom(src, "", d, &out); err == nil || !strings.Contains(err.Error(), "newnet") {
		t.Errorf("bad token: err = %v", err)
	}
	if err := enrollFrom(filepath.Join(src, "missing"), "", d, &out); err != nil {
		t.Errorf("a missing directory: %v", err)
	}
}

// In Kubernetes one Secret holds every pod's files, prefixed with the
// pod's name; a pod uses only its own.
func TestEnrollFromWithPrefix(t *testing.T) {
	dir := twoNetworkDir(t) // acme and partner
	src := t.TempDir()
	put := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(src, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	other, err := os.ReadFile(identityFile(t, "https://ctrl.other.example.com", time.Now().AddDate(1, 0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	put("zs-1.other.json", other)
	put("zs-10.other.json", other)             // another pod, not a prefix match
	put("zs-0.other.jwt", []byte("not a jwt")) // another pod's token: never touched
	put("zs-1.acme.jwt", []byte("not a jwt"))  // already there: never redeemed

	d, err := config.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := enrollFrom(src, "zs-1", d, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `Added network "other"`) {
		t.Errorf("output = %q", out.String())
	}
	if !strings.Contains(out.String(), "does not use (zs-10.other.json)") {
		t.Errorf("no warning about another pod's identity: %q", out.String())
	}
	d, _ = config.Read(dir)
	if _, ok := d.Network("zs-1.other"); ok {
		t.Error("the prefix was kept in the network name")
	}
}
