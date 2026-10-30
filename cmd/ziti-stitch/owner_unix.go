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
	"errors"
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
	uid, _, err := lookupUser(name)
	if err != nil {
		return err
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
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
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

// chownTo gives dir, and the regular files directly in it, to the user
// called name and their primary group. That user may own dir already (from
// an earlier install) and so could have put a link to any root-owned file
// in it: each entry is opened without following a symbolic link and changed
// through that handle, and only if it is a regular file with a single
// link. Anything else is left alone.
func chownTo(dir, name string) error {
	uid, gid, err := lookupUser(name)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if err := chownOpened(dir, true, uid, gid); err != nil {
		return err
	}
	for _, e := range entries {
		if err := chownOpened(filepath.Join(dir, e.Name()), false, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// chownOpened gives path to uid and gid if openForChown accepts it.
func chownOpened(path string, isDir bool, uid, gid int) error {
	f, err := openForChown(path, isDir)
	if err != nil || f == nil {
		return err
	}
	defer f.Close()
	return f.Chown(uid, gid)
}

// openForChown opens path without following a symbolic link, and returns
// it if it is what chownTo may change: a directory when isDir, else a
// regular file with a single link. It returns nil for anything else, and
// an error for a dir that is not one.
func openForChown(path string, isDir bool) (*os.File, error) {
	// O_NONBLOCK: opening a FIFO must not hang.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) && !isDir {
		return nil, nil // a symbolic link
	}
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case isDir && !fi.IsDir():
		f.Close()
		return nil, fmt.Errorf("%s is not a directory", path)
	case !isDir && (!fi.Mode().IsRegular() || !ok || st.Nlink != 1):
		f.Close()
		return nil, nil
	}
	return f, nil
}

// lookupUser returns the uid and primary gid of the user called name.
func lookupUser(name string) (uid, gid int, err error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("--user %s: %w", name, err)
	}
	u32, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("--user %s: unexpected uid %q", name, u.Uid)
	}
	g32, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("--user %s: unexpected gid %q", name, u.Gid)
	}
	return int(u32), int(g32), nil
}
