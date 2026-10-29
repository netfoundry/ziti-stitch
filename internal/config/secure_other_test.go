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
	"testing"
)

// A config directory that already exists loses write access for group and
// others, and keeps the rest of its mode.
func TestEnsureDirTightensAnExistingDir(t *testing.T) {
	for _, tc := range []struct{ before, after os.FileMode }{
		{0o777, 0o755},
		{0o770, 0o750},
		{0o750, 0o750},
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
