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

package network

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/sdk-golang/ziti/edge"
)

// establishedPoll is how often Listen checks whether a router has confirmed
// the bind.
const establishedPoll = 100 * time.Millisecond

// establishedCounter is implemented by the SDK's listener.
type establishedCounter interface {
	GetEstablishedCount() uint
}

// Listen binds service and waits up to timeout for a router to confirm a
// terminator, closing the listener if none does.
//
// It does what ListenOptions.WaitForNEstablishedListeners does, because the
// SDK's own wait (v1.9.1) can miss the confirmation: when a router's
// BindSuccess arrives before the new listener is registered, the
// established event counts zero, nothing re-checks, and the SDK times out
// and closes a listener that is up.
func Listen(ctx context.Context, zctx ziti.Context, service string, opts *ziti.ListenOptions, timeout time.Duration) (edge.Listener, error) {
	o := *opts
	o.WaitForNEstablishedListeners = 0
	l, err := zctx.ListenWithOptions(service, &o)
	if err != nil {
		return nil, err
	}
	counter, ok := l.(establishedCounter)
	if !ok {
		return l, nil // cannot tell; accepting will show whether it works
	}
	tick := time.NewTicker(establishedPoll)
	defer tick.Stop()
	deadline := time.After(timeout)
	for counter.GetEstablishedCount() == 0 {
		select {
		case <-tick.C:
			if l.IsClosed() {
				return nil, errors.New("the listener closed before a router confirmed the bind")
			}
		case <-deadline:
			_ = l.Close()
			return nil, fmt.Errorf("no router confirmed the bind within %v", timeout)
		case <-ctx.Done():
			_ = l.Close()
			return nil, ctx.Err()
		}
	}
	return l, nil
}
