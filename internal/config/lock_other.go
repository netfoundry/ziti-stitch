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
	"errors"
	"os"
	"syscall"
)

// tryLock flocks the directory itself, so there is no lock file to own or
// clean up. Locks on separate opens exclude each other within one process
// too.
func tryLock(path string) (unlock func(), err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
	case err == nil:
	case errors.Is(err, syscall.EWOULDBLOCK):
		f.Close()
		return nil, errLocked
	case errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOLCK):
		// A filesystem without locks (some network and FUSE mounts):
		// going without is better than every change, certificate
		// renewals included, failing until the certificate expires.
		return func() { f.Close() }, nil
	default:
		f.Close()
		return nil, err
	}
	// Closing the last descriptor of an open releases its lock.
	return func() { f.Close() }, nil
}
