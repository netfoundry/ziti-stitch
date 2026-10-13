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

// Package e2etest finds the two-network harness that deploy/local-e2e.sh
// starts, for the end-to-end tests (go test -tags e2e).
package e2etest

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Dir returns the harness's run directory, from ZITI_STITCH_E2E_DIR, and
// skips the test when it is not set.
func Dir(t testing.TB) string {
	t.Helper()
	d := os.Getenv("ZITI_STITCH_E2E_DIR")
	if d == "" {
		t.Skip("ZITI_STITCH_E2E_DIR not set; start ./deploy/local-e2e.sh up and run make e2e-test")
	}
	return d
}

// RequireCallerIdentity skips the test when the harness runs an OpenZiti
// version whose routers do not pass on the dialer's identity (before 2.0).
func RequireCallerIdentity(t testing.TB) {
	t.Helper()
	if v, ok := zitiVersion(Dir(t)); ok && !hasCallerIdentity(v) {
		t.Skipf("the harness runs OpenZiti %s; routers pass on the dialer identity from 2.0", v)
	}
}

// zitiVersion reads the version the harness recorded when it started.
func zitiVersion(dir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "ziti-version"))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// hasCallerIdentity reports whether OpenZiti version v ("v2.0.8") is 2.0
// or later.
func hasCallerIdentity(v string) bool {
	major, _, _ := strings.Cut(strings.TrimPrefix(v, "v"), ".")
	n, err := strconv.Atoi(major)
	return err == nil && n >= 2
}
