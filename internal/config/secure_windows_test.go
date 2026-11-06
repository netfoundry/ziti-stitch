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
	want := []string{"S-1-3-4", "S-1-5-18", "S-1-5-32-544"} // Owner Rights (read only), SYSTEM, Administrators
	for _, p := range []string{dir, file} {
		if got := allowedSIDs(t, p); !slices.Equal(got, want) {
			t.Errorf("%s: access granted to %v, want only %v", p, got, want)
		}
	}
	// Run again on the directory as it now is, as every start does.
	if err := EnsureDir(dir); err != nil {
		t.Errorf("a second EnsureDir: %v", err)
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if owner, _, err := sd.Owner(); err != nil || !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		t.Errorf("owner after EnsureDir = %v, %v; want Administrators", owner, err)
	}
}

func TestTrustedOwner(t *testing.T) {
	sid := func(k windows.WELL_KNOWN_SID_TYPE) *windows.SID {
		s, err := windows.CreateWellKnownSid(k)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	self := user.User.Sid
	for _, c := range []struct {
		name  string
		owner *windows.SID
		want  bool
	}{
		{"SYSTEM", sid(windows.WinLocalSystemSid), true},
		{"Administrators", sid(windows.WinBuiltinAdministratorsSid), true},
		{"this process's user", self, true},
		{"Users", sid(windows.WinBuiltinUsersSid), false},
		{"Everyone", sid(windows.WinWorldSid), false},
	} {
		if got := trustedOwner(c.owner, self); got != c.want {
			t.Errorf("owned by %s: trusted = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsAdministrator(t *testing.T) {
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	token := windows.GetCurrentProcessToken()
	elevated, err := token.IsMember(admins)
	if err != nil {
		t.Fatal(err)
	}
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := isAdministrator(user.User.Sid); err != nil || (elevated && !got) {
		t.Errorf("this process's user, an elevated administrator %v: isAdministrator = %v, %v", elevated, got, err)
	}
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := isAdministrator(users); err != nil || got {
		t.Errorf("the Users group: isAdministrator = %v, %v; want false", got, err)
	}
}

// A directory link is refused rather than secured: what it points at may
// belong to someone else.
func TestEnsureDirRefusesADirectoryLink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "cfg")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a directory link here: %v", err)
	}
	if err := EnsureDir(link); err == nil {
		t.Error("EnsureDir secured a directory link")
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
