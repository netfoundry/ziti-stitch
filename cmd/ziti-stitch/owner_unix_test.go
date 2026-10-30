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

package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCheckOwnedBy(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkOwnedBy(dir, me.Username); err != nil {
		t.Errorf("the owner was refused: %v", err)
	}
	if me.Uid == "0" {
		return // root owns everything here; no other user to compare with
	}
	if err := checkOwnedBy(dir, "root"); err == nil || !strings.Contains(err.Error(), "chown -R root: "+dir) {
		t.Errorf("err = %v, want the chown command", err)
	}
	if err := checkOwnedBy(dir, "no-such-user-zs"); err == nil {
		t.Error("an unknown user was accepted")
	}
}

func TestChownTo(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ziti-stitch.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Without root, only giving files to ourselves is allowed.
	if err := chownTo(dir, me.Username); err != nil {
		t.Fatal(err)
	}
	if err := checkOwnedBy(dir, me.Username); err != nil {
		t.Error(err)
	}
	if err := chownTo(dir, "no-such-user-zs"); err == nil {
		t.Error("an unknown user was accepted")
	}
}

// The log directory may belong to the service user already, who could have
// linked a root-owned file into it: only regular files with one link are
// given away, and links are not followed.
func TestOpenForChown(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	write := func(path string) {
		t.Helper()
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "ziti-stitch.log"))
	victim := filepath.Join(outside, "sudoers")
	write(victim)
	if err := os.Link(victim, filepath.Join(dir, "hard")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "soft")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]bool{"ziti-stitch.log": true, "hard": false, "soft": false, "fifo": false} {
		f, err := openForChown(filepath.Join(dir, name), false)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got := f != nil; got != want {
			t.Errorf("%s: opened for chown = %v, want %v", name, got, want)
		}
		if f != nil {
			f.Close()
		}
	}
	if f, err := openForChown(dir, true); err != nil || f == nil {
		t.Errorf("the directory itself: %v", err)
	} else {
		f.Close()
	}
	if _, err := openForChown(filepath.Join(dir, "ziti-stitch.log"), true); err == nil {
		t.Error("a file was accepted as the directory")
	}
}
