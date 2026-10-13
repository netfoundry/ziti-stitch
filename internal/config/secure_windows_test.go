//go:build windows

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
	"slices"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestEnsureDirRestrictsTheACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	// A file created afterwards must inherit the restriction.
	file := filepath.Join(dir, NetworksDirName, "acme.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []string{"S-1-5-18", "S-1-5-32-544"} // SYSTEM, Administrators
	for _, p := range []string{dir, file} {
		if got := allowedSIDs(t, p); !slices.Equal(got, want) {
			t.Errorf("%s: access granted to %v, want only %v", p, got, want)
		}
	}
}

// allowedSIDs returns the SIDs path's DACL grants access to, sorted. It
// fails the test on any ACE that is not an allow.
func allowedSIDs(t *testing.T, path string) []string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	var sids []string
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Errorf("%s: ACE %d has type %d, not an allow", path, i, ace.Header.AceType)
		}
		sids = append(sids, (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String())
	}
	slices.Sort(sids)
	return sids
}
