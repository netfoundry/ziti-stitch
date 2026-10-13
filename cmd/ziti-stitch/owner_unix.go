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
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// checkOwnedBy reports an error unless the user called name owns dir and
// everything in it: the service runs as that user and must read its
// identities and write renewed certificates.
func checkOwnedBy(dir, name string) error {
	u, err := user.Lookup(name)
	if err != nil {
		return fmt.Errorf("--user %s: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return fmt.Errorf("--user %s: unexpected uid %q", name, u.Uid)
	}
	var notOwned string
	err = filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && uint64(st.Uid) != uid {
			notOwned = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return err
	}
	if notOwned != "" {
		return fmt.Errorf("%s is not owned by %s, so the service could not read or update it; run: chown -R %s: %s", notOwned, name, name, dir)
	}
	return nil
}
