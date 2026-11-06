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

// Package atomicfile writes files so that readers see either the old
// content or the new content, never a partial write, even across a crash.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// WriteFile writes data to a temporary file in the same directory as path,
// syncs it, and renames it over path. The directory is synced afterwards
// (where the OS supports it) so the rename itself survives a crash.
//
// Once the rename is done the write has happened, and WriteFile reports
// success: a failed directory sync only risks the old content coming back
// after a crash, and reporting failure would have callers throw away what
// they saved (an identity whose one-time token is spent, say).
func WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+tempMark+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	_ = syncDir(dir)
	return nil
}

// Rename renames oldpath to newpath and syncs the directory, so that a
// completed rename is durable. As with WriteFile, a rename that happened
// is reported as done.
func Rename(oldpath, newpath string) error {
	if err := os.Rename(oldpath, newpath); err != nil {
		return err
	}
	_ = syncDir(filepath.Dir(newpath))
	return nil
}

// tempMark is in the name of WriteFile's temporary files.
const tempMark = ".tmp-"

// RemoveStale removes WriteFile's temporary files in dir that are older
// than age: ones a crash left behind, which may hold what was being
// written (a private key). Younger ones may still be on their way in.
func RemoveStale(dir string, age time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, ".") || !strings.Contains(name, tempMark) {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() && time.Since(fi.ModTime()) > age {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

func syncDir(dir string) error {
	// Windows cannot open a directory for syncing; NTFS metadata updates
	// from MoveFileEx are journaled, which is the best available there.
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
