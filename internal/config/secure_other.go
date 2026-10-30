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

// securePrivateDir takes write access to path, and to its networks
// directory, away from group and others. EnsureDir creates them with mode
// 0700, but directories that already existed keep their mode, and anyone
// else who can write to one can swap what is in it for their own: an
// identity in networks/, or web/ with a login token planted in it. Read
// access is left alone: what it shows is not secret, identity files being
// 0600.
//
// Only a directory this process owns, or any as root, is changed. Another
// user's directory is theirs to secure: a Kubernetes volume, for one, is
// owned by root and writable by the pod's group or everyone, with no other
// users to fear.
func securePrivateDir(path string) error {
	for _, dir := range []string{path, filepath.Join(path, NetworksDirName)} {
		if err := dropGroupOtherWrite(dir); err != nil {
			return fmt.Errorf("restricting access to %s: %w", dir, err)
		}
	}
	return nil
}

// dropGroupOtherWrite works on an open handle, so that the directory
// checked is the one changed even if the path is swapped meanwhile, and
// never sets setuid, which an owner may put on a directory to have it
// carried onto whatever the path was swapped for.
func dropGroupOtherWrite(dir string) error {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o022 == 0 {
		return nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if euid := os.Geteuid(); !ok || (euid != 0 && uint64(st.Uid) != uint64(euid)) {
		return nil
	}
	// fi.Mode keeps setgid and sticky, which Chmod applies too.
	return f.Chmod(fi.Mode() &^ 0o022 &^ os.ModeSetuid)
}
