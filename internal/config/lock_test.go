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
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUpdatesAtOnceAreAllSaved(t *testing.T) {
	dir := writeDir(t, twoNetworks(""))
	const n = 20
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			errs <- Update(dir, func(d *Dir) error {
				d.Stitches = append(d.Stitches, Stitch{
					Name: fmt.Sprintf("s%d", i),
					From: Ingress{Network: "acme", Service: fmt.Sprintf("a%d", i)},
					To:   Egress{Network: "partner", Service: "b"},
				}.WithDefaults())
				return nil
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	d, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Stitches) != n {
		t.Errorf("%d of %d stitches saved; the others were lost", len(d.Stitches), n)
	}
}

func TestLockWaitsForTheHolder(t *testing.T) {
	dir := t.TempDir()
	defer func(w time.Duration) { lockWait = w }(lockWait)
	lockWait = 200 * time.Millisecond

	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); err == nil || !strings.Contains(err.Error(), "still in progress") {
		t.Fatalf("second Lock: err = %v, want still in progress", err)
	}

	time.AfterFunc(50*time.Millisecond, unlock)
	unlock, err = Lock(dir)
	if err != nil {
		t.Fatalf("Lock after the holder let go: %v", err)
	}
	unlock()
}
