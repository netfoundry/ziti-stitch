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
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/network"
	"github.com/netfoundry/ziti-stitch/internal/stitch"
)

// drainTimeout is how long shutdown waits for open connections to finish
// before closing them.
const drainTimeout = 30 * time.Second

func newRunCmd(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run every enabled stitch until stopped",
		Long: `Run every enabled stitch until interrupted (SIGINT or SIGTERM).

On shutdown, ziti-stitch stops accepting new connections, waits up to 30s
for open ones to finish, then closes the rest. A second interrupt exits
immediately.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := resolveConfigDir(*configDir)
			if err != nil {
				return err
			}
			d, err := config.Load(dir)
			if err != nil {
				return err
			}
			log := newLogger(d.Config.Log, cmd.ErrOrStderr())

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			run(ctx, stop, d, log)
			return nil
		},
	}
}

// run starts the networks and stitches in d and blocks until ctx is done,
// then shuts down in order: stop listening, drain connections, close the
// networks. stopSignals is called once shutdown starts so that a second
// interrupt gets the default behavior and kills the process.
func run(ctx context.Context, stopSignals func(), d *config.Dir, log *slog.Logger) {
	var enabled []config.Stitch
	used := map[string]bool{}
	for _, s := range d.Stitches {
		if s.Disabled {
			continue
		}
		enabled = append(enabled, s)
		used[s.From.Network] = true
		used[s.To.Network] = true
	}

	// Networks outlive the stitches' context: connections draining after
	// shutdown starts still need them.
	netCtx, closeNetworks := context.WithCancel(context.Background())
	defer closeNetworks()
	var netsRunning sync.WaitGroup
	nets := map[string]*network.Network{}
	for _, n := range d.Networks {
		if !used[n.Name] {
			continue
		}
		nw := network.New(n.Name, n.IdentityFile, log)
		nets[n.Name] = nw
		netsRunning.Go(func() { nw.Run(netCtx) })
	}

	// Stitches get their own context so that shutdown is logged before
	// they react to it.
	stitchCtx, stopStitches := context.WithCancel(context.Background())
	defer stopStitches()
	var listening sync.WaitGroup
	stitches := make([]*stitch.Stitch, 0, len(enabled))
	for _, s := range enabled {
		st := stitch.New(s, nets[s.From.Network], nets[s.To.Network], log)
		stitches = append(stitches, st)
		listening.Go(func() { st.Run(stitchCtx) })
	}

	log.Info("ziti-stitch running", "version", version, "config_dir", d.Path,
		"stitches", len(enabled), "disabled_stitches", len(d.Stitches)-len(enabled), "networks", len(nets))
	if len(enabled) == 0 {
		log.Warn("no enabled stitches; nothing to do until some are configured")
	}

	<-ctx.Done()
	stopSignals()
	log.Info("shutting down: no new connections; draining open ones", "timeout", drainTimeout)
	stopStitches()
	listening.Wait()

	drained := make(chan struct{})
	go func() {
		for _, st := range stitches {
			st.Wait()
		}
		close(drained)
	}()
	select {
	case <-drained:
		log.Info("all connections finished")
	case <-time.After(drainTimeout):
		log.Warn("drain timeout; closing the remaining connections")
	}

	closeNetworks()
	netsRunning.Wait()
	log.Info("stopped")
}
