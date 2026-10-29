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
