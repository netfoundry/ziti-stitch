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
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/runner"
)

// drainTimeout is how long shutdown waits for open connections to finish
// before closing them.
const drainTimeout = 30 * time.Second

func newRunCmd(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run every enabled stitch until stopped",
		Long: `Run every enabled stitch until interrupted (SIGINT or SIGTERM).

Changes to the config directory, from the CLI or by hand, are applied as
they happen (SIGHUP forces a reload). Stitches that did not change are not
touched; removed or changed ones stop accepting and let their open
connections finish. An invalid change is logged and not applied.

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
			log, setLevel := newLogger(d.Config.Log, cmd.ErrOrStderr())

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			run(ctx, stop, d, log, setLevel)
			return nil
		},
	}
}

// run applies d and keeps applying changes to it until ctx is done, then
// shuts down. stopSignals is called once shutdown starts so that a second
// interrupt gets the default behavior and kills the process.
func run(ctx context.Context, stopSignals func(), d *config.Dir, log *slog.Logger, setLevel func(string)) {
	r := runner.New(log)
	r.Apply(d)
	logRunning(log, d)

	reload := make(chan struct{}, 1)
	requestReload := func() {
		select {
		case reload <- struct{}{}:
		default: // one is already pending
		}
	}
	// SIGHUP never arrives on Windows; the watcher covers it there.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	if err := watchConfig(ctx, d.Path, requestReload, log); err != nil {
		log.Warn("cannot watch the config directory; send SIGHUP to apply changes", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			stopSignals()
			log.Info("shutting down: no new connections; draining open ones", "timeout", drainTimeout)
			if r.Shutdown(drainTimeout) {
				log.Info("all connections finished")
			} else {
				log.Warn("drain timeout; closed the remaining connections")
			}
			log.Info("stopped")
			return
		case <-hup:
			d = reloadConfig(d, r, log, setLevel)
		case <-reload:
			d = reloadConfig(d, r, log, setLevel)
		}
	}
}

func logRunning(log *slog.Logger, d *config.Dir) {
	enabled := 0
	for _, s := range d.Stitches {
		if !s.Disabled {
			enabled++
		}
	}
	log.Info("ziti-stitch running", "version", version, "config_dir", d.Path,
		"stitches", enabled, "disabled_stitches", len(d.Stitches)-enabled)
	if enabled == 0 {
		log.Warn("no enabled stitches; nothing to do until some are configured")
	}
}

// reloadConfig reads the config again and applies it. An invalid config is
// not applied; the running one stays. It returns the config now in force.
func reloadConfig(cur *config.Dir, r *runner.Runner, log *slog.Logger, setLevel func(string)) *config.Dir {
	d, err := config.Load(cur.Path)
	if err != nil {
		log.Error("configuration change not applied; still running the previous configuration", "error", err)
		return cur
	}
	if d.Config.Log.Level != cur.Config.Log.Level {
		setLevel(d.Config.Log.Level)
		log.Info("log level changed", "level", d.Config.Log.Level)
	}
	if d.Config.Log.Format != cur.Config.Log.Format {
		log.Warn("log.format changes take effect after a restart", "format", cur.Config.Log.Format)
	}
	if c := r.Apply(d); c != (runner.Changes{}) {
		log.Info("configuration applied", "started", c.Started, "restarted", c.Restarted, "stopped", c.Stopped)
	} else {
		log.Debug("configuration reloaded; no stitch changed")
	}
	return d
}
