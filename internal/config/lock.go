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
	"fmt"
	"time"
)

// errLocked is what tryLock returns while someone else holds the lock.
var errLocked = errors.New("locked")

// lockWait is how long Lock waits for another change to finish.
var lockWait = 10 * time.Second

// lockPoll is how often Lock tries again meanwhile.
const lockPoll = 50 * time.Millisecond

// Lock takes the edit lock of the config directory at path, waiting a
// while if another change holds it. The CLI and the web UI read the
// directory only once they hold the lock, so changes they make at the same
// time, from separate processes too, are applied one after the other and
// none is lost. Reading needs no lock: files are replaced atomically.
// unlock releases the lock; so does the process exiting.
func Lock(path string) (unlock func(), err error) {
	deadline := time.Now().Add(lockWait)
	for {
		unlock, err = tryLock(path)
		if !errors.Is(err, errLocked) {
			return unlock, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another change to %s is still in progress; try again", path)
		}
		time.Sleep(lockPoll)
	}
}

// Update takes the edit lock of the config directory at path, reads the
// directory and applies change through Edit.
func Update(path string, change func(d *Dir) error) error {
	unlock, err := Lock(path)
	if err != nil {
		return err
	}
	defer unlock()
	d, err := Read(path)
	if err != nil {
		return err
	}
	return d.Edit(change)
}
