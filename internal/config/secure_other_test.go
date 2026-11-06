//go:build !windows

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
	"strings"
	"testing"
)

// A config directory that already exists loses all access for group and
// others, and keeps the rest of its mode.
func TestEnsureDirTightensAnExistingDir(t *testing.T) {
	for _, tc := range []struct{ before, after os.FileMode }{
		{0o777, 0o700},
		{0o750, 0o700},
		{0o705, 0o700},
		{0o700, 0o700},
	} {
		dir := filepath.Join(t.TempDir(), "cfg")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, tc.before); err != nil { // not subject to the umask
			t.Fatal(err)
		}
		if err := EnsureDir(dir); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != tc.after {
			t.Errorf("%v: mode after EnsureDir = %v, want %v", tc.before, got, tc.after)
		}
	}
}

// networks/ holds the identities: one that already existed loses group
// and other access too.
func TestEnsureDirTightensAnExistingNetworksDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	networks := filepath.Join(dir, NetworksDirName)
	if err := os.MkdirAll(networks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(networks, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(networks); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("networks mode = %v, %v; want 0700", fi.Mode().Perm(), err)
	}
}

// networks/ as a link is refused: run as root, securing it would change
// whatever it points at.
func TestEnsureDirRefusesALinkedNetworksDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	for _, d := range []string{dir, elsewhere} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(elsewhere, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, NetworksDirName)); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(dir); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("EnsureDir = %v, want a refusal of the link", err)
	}
	if fi, err := os.Stat(elsewhere); err != nil || fi.Mode().Perm() != 0o777 {
		t.Errorf("the link's target was changed: mode %v, %v", fi.Mode().Perm(), err)
	}
}

// setuid is never carried over: an owner can set it on a directory.
func TestEnsureDirDropsSetuid(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode()&os.ModeSetuid == 0 {
		t.Skip("this system does not set setuid on a directory")
	}
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode()&os.ModeSetuid != 0 || fi.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, %v; want 0700 without setuid", fi.Mode(), err)
	}
}
