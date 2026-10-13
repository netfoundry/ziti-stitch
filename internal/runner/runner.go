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

// Package runner keeps the running networks and stitches in line with the
// configuration, applying changes without disturbing what did not change.
package runner

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/network"
	"github.com/netfoundry/ziti-stitch/internal/stitch"
)

// Network is a network as the runner drives it.
type Network interface {
	stitch.Network
	// Run runs until ctx is done, then releases everything.
	Run(ctx context.Context)
}

// Stitch is a stitch as the runner drives it.
type Stitch interface {
	// Run serves until ctx is done; connections it accepted carry on.
	Run(ctx context.Context)
	// Wait blocks until those connections have finished.
	Wait()
}

// Runner owns the running networks and stitches.
//
// A stitch that is removed or changed stops accepting at once, and its
// open connections drain in the background; a changed one is replaced
// immediately. A network runs while any stitch uses it, draining ones
// included, and stops once none does.
type Runner struct {
	log        *slog.Logger
	newNetwork func(name, path string) Network
	newStitch  func(cfg config.Stitch, from, to Network) Stitch

	mu       sync.Mutex
	networks map[string]*netEntry    // by name; the current entry for each
	live     map[*netEntry]bool      // every network not yet stopped, retired ones included
	stitches map[string]*stitchEntry // by name; running ones only

	draining    sync.WaitGroup // stopped stitches still finishing connections
	netsRunning sync.WaitGroup
}

type netEntry struct {
	name, path string
	nw         Network
	cancel     context.CancelFunc
	users      int  // stitches using it, draining ones included
	wanted     bool // the current config uses it
	stopped    bool
}

type stitchEntry struct {
	cfg      config.Stitch
	st       Stitch
	from, to *netEntry
	cancel   context.CancelFunc
	done     chan struct{} // closed when Run returns
}

// New returns a runner with nothing running.
func New(log *slog.Logger) *Runner {
	return newRunner(log,
		func(name, path string) Network { return network.New(name, path, log) },
		func(cfg config.Stitch, from, to Network) Stitch { return stitch.New(cfg, from, to, log) })
}

func newRunner(log *slog.Logger, newNetwork func(name, path string) Network, newStitch func(cfg config.Stitch, from, to Network) Stitch) *Runner {
	return &Runner{
		log:        log,
		newNetwork: newNetwork,
		newStitch:  newStitch,
		networks:   map[string]*netEntry{},
		live:       map[*netEntry]bool{},
		stitches:   map[string]*stitchEntry{},
	}
}

// Changes counts what an Apply did.
type Changes struct {
	Started, Restarted, Stopped int
}

// Apply makes the running stitches match the enabled stitches in d, which
// must be valid. Stitches whose configuration and networks are unchanged
// are not touched.
func (r *Runner) Apply(d *config.Dir) Changes {
	r.mu.Lock()
	defer r.mu.Unlock()

	desired := map[string]config.Stitch{}
	for _, s := range d.Stitches {
		if !s.Disabled {
			desired[s.Name] = s
		}
	}
	paths := map[string]string{}
	for _, n := range d.Networks {
		paths[n.Name] = n.IdentityFile
	}

	var c Changes
	restarting := map[string]bool{}
	for name, e := range r.stitches {
		want, ok := desired[name]
		switch {
		case !ok:
			r.log.Info("stitch removed or disabled; stopping it", "stitch", name)
			c.Stopped++
		case want != e.cfg || e.from.path != paths[want.From.Network] || e.to.path != paths[want.To.Network]:
			r.log.Info("stitch changed; restarting it", "stitch", name)
			restarting[name] = true
		default:
			continue
		}
		r.stopStitch(e)
	}

	for _, n := range r.networks {
		n.wanted = false
	}
	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		want := desired[name]
		from := r.network(want.From.Network, paths[want.From.Network])
		to := r.network(want.To.Network, paths[want.To.Network])
		from.wanted, to.wanted = true, true
		if _, running := r.stitches[name]; running {
			continue
		}
		r.startStitch(want, from, to)
		if restarting[name] {
			c.Restarted++
		} else {
			c.Started++
		}
	}

	for _, n := range r.networks {
		if !n.wanted && n.users == 0 {
			r.stopNetwork(n)
		}
	}
	return c
}

// Shutdown stops every stitch, waits up to timeout for their connections
// to finish, then stops every network (which ends whatever is left). It
// reports whether everything finished in time.
func (r *Runner) Shutdown(timeout time.Duration) bool {
	r.mu.Lock()
	for _, e := range r.stitches {
		r.stopStitch(e)
	}
	for _, n := range r.networks {
		n.wanted = false
		if n.users == 0 {
			r.stopNetwork(n)
		}
	}
	r.mu.Unlock()

	drained := make(chan struct{})
	go func() {
		r.draining.Wait()
		close(drained)
	}()
	inTime := true
	select {
	case <-drained:
	case <-time.After(timeout):
		inTime = false
		r.mu.Lock()
		for n := range r.live {
			r.stopNetwork(n)
		}
		r.mu.Unlock()
	}
	r.netsRunning.Wait()
	return inTime
}

// network returns the running network called name, starting it if needed.
// A network whose identity file moved is replaced; the old one keeps
// running until the stitches still using it have drained. The caller
// holds mu.
func (r *Runner) network(name, path string) *netEntry {
	if n, ok := r.networks[name]; ok {
		if n.path == path {
			return n
		}
		delete(r.networks, name) // retired; release stops it once unused
		if n.users == 0 {
			r.stopNetwork(n)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &netEntry{name: name, path: path, nw: r.newNetwork(name, path), cancel: cancel}
	r.networks[name] = n
	r.live[n] = true
	r.netsRunning.Go(func() { n.nw.Run(ctx) })
	r.log.Info("network started", "network", name)
	return n
}

// startStitch runs a stitch. The caller holds mu.
func (r *Runner) startStitch(cfg config.Stitch, from, to *netEntry) {
	ctx, cancel := context.WithCancel(context.Background())
	e := &stitchEntry{cfg: cfg, st: r.newStitch(cfg, from.nw, to.nw), from: from, to: to, cancel: cancel, done: make(chan struct{})}
	r.stitches[cfg.Name] = e
	from.users++
	to.users++
	go func() {
		e.st.Run(ctx)
		close(e.done)
	}()
}

// stopStitch stops a stitch accepting and lets its connections drain in
// the background, then releases its networks. The caller holds mu.
func (r *Runner) stopStitch(e *stitchEntry) {
	delete(r.stitches, e.cfg.Name)
	e.cancel()
	r.draining.Go(func() {
		<-e.done
		e.st.Wait()
		r.mu.Lock()
		defer r.mu.Unlock()
		r.release(e.from)
		r.release(e.to)
	})
}

// release drops one use of n, stopping it if nothing needs it any more.
// The caller holds mu.
func (r *Runner) release(n *netEntry) {
	n.users--
	if n.users == 0 && (!n.wanted || r.networks[n.name] != n) {
		r.stopNetwork(n)
	}
}

// stopNetwork stops n. Stopping twice is harmless. The caller holds mu.
func (r *Runner) stopNetwork(n *netEntry) {
	if n.stopped {
		return
	}
	n.stopped = true
	if r.networks[n.name] == n {
		delete(r.networks, n.name)
	}
	delete(r.live, n)
	n.cancel()
	r.log.Info("network stopped", "network", n.name)
}
