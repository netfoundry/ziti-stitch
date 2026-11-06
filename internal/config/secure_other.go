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
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// securePrivateDir takes group and other access away from path, and from
// its networks directory, leaving them as EnsureDir creates them (mode
// 0700). Directories that already existed keep their mode otherwise, and
// anyone else who can write to one can swap what is in it for their own
// (an identity in networks/, or web/ with a login token planted in it),
// while anyone who can open one can hold its edit lock (see tryLock) and
// stall every change and certificate renewal.
//
// Only a directory this process owns, or any as root, is changed. Another
// user's directory is theirs to secure: a Kubernetes volume, for one, is
// owned by root and writable by the pod's group or everyone, with no other
// users to fear. networks/ must be a directory, not a link: run as root,
// following one would change whatever it points at.
func securePrivateDir(path string) error {
	if err := dropGroupOtherAccess(path, 0); err != nil {
		return fmt.Errorf("restricting access to %s: %w", path, err)
	}
	networks := filepath.Join(path, NetworksDirName)
	if err := dropGroupOtherAccess(networks, syscall.O_NOFOLLOW); err != nil {
		if fi, lerr := os.Lstat(networks); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link; it must be a directory", networks)
		}
		return fmt.Errorf("restricting access to %s: %w", networks, err)
	}
	return nil
}

// dropGroupOtherAccess opens dir with the extra open flags and works on
// the handle, so that the directory checked is the one changed even if the
// path is swapped meanwhile. It never sets setuid, which an owner may put
// on a directory to have it carried onto whatever the path was swapped for.
func dropGroupOtherAccess(dir string, flags int) error {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|flags, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if euid := os.Geteuid(); !ok || (euid != 0 && uint64(st.Uid) != uint64(euid)) {
		return nil
	}
	// fi.Mode keeps setgid and sticky, which Chmod applies too.
	return f.Chmod(fi.Mode() &^ 0o077 &^ os.ModeSetuid)
}
