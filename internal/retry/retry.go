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

// Package retry has the backoff used by everything that waits for a
// network, a permission or a service to come back.
package retry

import (
	"context"
	"time"
)

// Backoff doubles a delay from Min up to Max. The zero value is not
// useful; set Min and Max.
type Backoff struct {
	Min, Max time.Duration
	cur      time.Duration
}

// Next returns the delay to wait now and advances the backoff.
func (b *Backoff) Next() time.Duration {
	if b.cur == 0 {
		b.cur = b.Min
	}
	d := b.cur
	b.cur = min(b.cur*2, b.Max)
	return d
}

// Reset starts the next Next over at Min.
func (b *Backoff) Reset() {
	b.cur = 0
}

// Sleep waits for d or until ctx is done, and reports whether the full
// wait happened.
func Sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
