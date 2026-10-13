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

// Package network owns one Ziti identity per network: authenticating it,
// keeping its certificate renewed, and saving controller list changes back
// to its identity file.
package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openziti/sdk-golang/ziti"

	"github.com/netfoundry/ziti-stitch/internal/atomicfile"
	"github.com/netfoundry/ziti-stitch/internal/retry"
)

// PendingSuffix marks an identity file written during renewal, before the
// controller has confirmed the new certificate. See Network.renew.
const PendingSuffix = ".pending"

// identityFilePerm keeps private keys readable by the owner only.
const identityFilePerm = 0o600

// serviceRefreshInterval is how often the SDK checks whether our service
// list changed (a cheap call when it has not). The SDK default is five
// minutes, during which a granted or restored permission goes unnoticed
// and dials and binds fail against the stale list.
const serviceRefreshInterval = 30 * time.Second

// Network is one enrolled identity and the Ziti context built from it.
type Network struct {
	name string
	path string
	log  *slog.Logger

	// fileMu serializes rewrites of the identity file (renewal and
	// controller list updates both rewrite it).
	fileMu sync.Mutex
	// urlsSeq numbers controller list updates so that a save which lost
	// the race for fileMu to a newer one does not overwrite it.
	urlsSeq atomic.Uint64

	ready chan struct{} // closed once zctx is set
	zctx  ziti.Context
}

// New returns a network for the identity file at path. Nothing happens
// until Run.
func New(name, path string, log *slog.Logger) *Network {
	return &Network{
		name:  name,
		path:  path,
		log:   log.With("network", name),
		ready: make(chan struct{}),
	}
}

// Name returns the network's name.
func (n *Network) Name() string {
	return n.name
}

// Context waits until the network has authenticated, then returns its Ziti
// context. It fails only when ctx is done first.
func (n *Network) Context(ctx context.Context) (ziti.Context, error) {
	select {
	case <-n.ready:
		return n.zctx, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Run authenticates, retrying until it succeeds, then keeps the identity's
// certificate renewed until ctx is done. It closes the Ziti context on the
// way out.
func (n *Network) Run(ctx context.Context) {
	zctx, ok := n.connect(ctx)
	if !ok {
		return
	}
	defer zctx.Close()

	if l, ok := zctx.(controllerURLsNotifier); ok {
		remove := l.AddControllerUrlsUpdateListener(func(_ ziti.Context, urls []*url.URL) {
			// Off the SDK's event goroutine: renewal can hold fileMu across
			// controller calls.
			go n.saveControllerURLs(n.urlsSeq.Add(1), urls)
		})
		defer remove()
	} else {
		n.log.Warn("Ziti context cannot report controller list changes; they will not be saved to the identity file")
	}

	n.zctx = zctx
	close(n.ready)
	n.keepRenewed(ctx, zctx)
}

// controllerURLsNotifier is implemented by the SDK's context but is not
// part of ziti.Context.
type controllerURLsNotifier interface {
	AddControllerUrlsUpdateListener(func(ziti.Context, []*url.URL)) func()
}

// connect authenticates with the identity file, retrying with backoff
// until it works or ctx is done.
//
// A leftover pending file means a renewal was interrupted, and nothing on
// disk says whether the controller confirmed the new certificate (the old
// one stops working the moment it does). So both are tried, newest first:
// whichever authenticates is the live one. When neither does, for instance
// because the controller is unreachable, both are kept and tried again.
func (n *Network) connect(ctx context.Context) (ziti.Context, bool) {
	pending := n.pendingPath()
	b := retry.Backoff{Min: time.Second, Max: time.Minute}
	for attempt := 1; ; attempt++ {
		candidates := []string{n.path}
		if _, err := os.Stat(pending); err == nil {
			candidates = []string{pending, n.path}
		}

		var errs []error
		for _, file := range candidates {
			zctx, err := authenticate(file)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", file, err))
				continue
			}
			if err := n.settlePending(file, pending); err != nil {
				zctx.Close()
				errs = append(errs, err)
				break
			}
			n.log.Info("authenticated", "attempts", attempt)
			return zctx, true
		}

		wait := b.Next()
		n.log.Error("cannot authenticate; retrying", "error", errors.Join(errs...), "retry_in", wait)
		if !retry.Sleep(ctx, wait) {
			return nil, false
		}
	}
}

// settlePending finishes an interrupted renewal once we know which file
// authenticated: a working pending file replaces the identity file; a
// working identity file means the pending one was never confirmed.
func (n *Network) settlePending(worked, pending string) error {
	if worked == pending {
		if err := atomicfile.Rename(pending, n.path); err != nil {
			return fmt.Errorf("finishing interrupted certificate renewal: %w", err)
		}
		n.log.Warn("finished an interrupted certificate renewal")
		return nil
	}
	if err := os.Remove(pending); err == nil {
		n.log.Warn("discarded a certificate renewal the controller never confirmed")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing unconfirmed renewal: %w", err)
	}
	return nil
}

func authenticate(file string) (ziti.Context, error) {
	cfg, err := ziti.NewConfigFromFile(file)
	if err != nil {
		return nil, err
	}
	opts := *ziti.DefaultOptions
	opts.RefreshInterval = serviceRefreshInterval
	zctx, err := ziti.NewContextWithOpts(cfg, &opts)
	if err != nil {
		return nil, err
	}
	if err := zctx.Authenticate(); err != nil {
		zctx.Close()
		return nil, err
	}
	return zctx, nil
}

// saveControllerURLs records a new controller list in the identity file,
// so that a restart after the original controller is retired can still
// find the network. seq is the update's number from urlsSeq.
func (n *Network) saveControllerURLs(seq uint64, urls []*url.URL) {
	list := make([]string, len(urls))
	for i, u := range urls {
		list[i] = u.String()
	}

	n.fileMu.Lock()
	defer n.fileMu.Unlock()
	if seq != n.urlsSeq.Load() {
		return // a newer list arrived while this one waited
	}
	raw, err := os.ReadFile(n.path)
	if err != nil {
		n.log.Error("cannot save controller list", "error", err)
		return
	}
	out, changed, err := setControllerURLs(raw, list)
	if err != nil {
		n.log.Error("cannot save controller list", "error", err)
		return
	}
	if !changed {
		return
	}
	if err := atomicfile.WriteFile(n.path, out, identityFilePerm); err != nil {
		n.log.Error("cannot save controller list", "error", err)
		return
	}
	n.log.Info("saved new controller list", "controllers", list)
}
