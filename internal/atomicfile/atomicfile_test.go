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

package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestWriteFileReplacesContentAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "id.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("perm = %o, want 600", perm)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want only the target file", len(entries))
	}
}

func TestWriteFileFailsWhenDirectoryMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "id.json")
	if err := WriteFile(path, []byte("x"), 0o600); err == nil {
		t.Fatal("expected an error for a missing directory")
	}
}

func TestRenameMovesFile(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "a.pending"), filepath.Join(dir, "a")
	if err := os.WriteFile(from, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Rename(from, to); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	got, err := os.ReadFile(to)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pending" {
		t.Errorf("content = %q, want %q", got, "pending")
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Errorf("source still exists (err=%v)", err)
	}
}

// Temporary files a crash left behind are removed once old enough; ones
// still being written, and other files, are not.
func TestRemoveStale(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	files := map[string]bool{ // name: removed
		".acme.json.tmp-123": true,
		".acme.json.tmp-456": false, // recent
		"acme.json":          false,
		".acme.json.pending": false,
		"notes.tmp-1":        false,
	}
	for name := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if name != ".acme.json.tmp-456" {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	RemoveStale(dir, time.Hour)
	for name, removed := range files {
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) != removed {
			t.Errorf("%s: removed = %v, want %v", name, os.IsNotExist(err), removed)
		}
	}
	RemoveStale(filepath.Join(dir, "missing"), time.Hour) // no directory: nothing to do
}
