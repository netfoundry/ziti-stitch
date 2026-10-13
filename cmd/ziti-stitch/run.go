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
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/events"
	"github.com/netfoundry/ziti-stitch/internal/runner"
	"github.com/netfoundry/ziti-stitch/internal/web"
)

// drainTimeout is how long shutdown waits for open connections to finish
// before closing them.
const drainTimeout = 30 * time.Second

// eventsKept is how many recent log records the web UI can show.
const eventsKept = 2000

func newRunCmd(configDir *string) *cobra.Command {
	var logFile string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run every enabled stitch until stopped",
		Long: `Run every enabled stitch until interrupted (SIGINT or SIGTERM).

Changes to the config directory, from the CLI or by hand, are applied as
they happen (SIGHUP forces a reload). Stitches that did not change are not
touched; removed or changed ones stop accepting and let their open
connections finish. An invalid change is logged and not applied; an invalid
configuration at startup is logged, and ziti-stitch waits for it to be
fixed.

On shutdown, ziti-stitch stops accepting new connections, waits up to 30s
for open ones to finish, then closes the rest. A second interrupt exits
immediately.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := resolveConfigDir(*configDir)
			if err != nil {
				return err
			}
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				return fmt.Errorf("no config directory at %s; start with: ziti-stitch enroll --jwt <file>", dir)
			}

			// An invalid config does not stop us: under a service manager
			// exiting would only mean a restart loop. Run nothing and wait
			// for the fix, exactly as for a bad change while running.
			d, loadErr := config.Load(dir)
			if loadErr != nil {
				d = &config.Dir{Path: dir, Config: config.Config{Log: config.Log{Level: "info", Format: "text"}}}
			}

			var w io.Writer = cmd.ErrOrStderr()
			if logFile != "" {
				lf, err := openLogFile(logFile)
				if err != nil {
					return err
				}
				defer lf.Close()
				w = lf
			}
			rec := events.NewRecorder(eventsKept)
			log, setLevel := newLogger(d.Config.Log, w, rec)
			if loadErr != nil {
				log.Error("configuration is not valid; running nothing until it is fixed", "error", loadErr)
			}

			// The Windows service control manager talks to services through
			// its own API, not signals.
			if runtime.GOOS == "windows" && !service.Interactive() {
				s, err := service.New(&program{run: func(ctx context.Context) {
					run(ctx, func() {}, d, log, setLevel, rec)
				}}, &service.Config{Name: serviceName})
				if err != nil {
					return err
				}
				return s.Run()
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			run(ctx, stop, d, log, setLevel, rec)
			return nil
		},
	}
	cmd.Flags().StringVar(&logFile, "log-file", "", "log to this file, rotated at 10 MB with 5 kept, instead of stderr")
	return cmd
}

// openLogFile returns a size-rotated log file at path.
func openLogFile(path string) (*lumberjack.Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	return &lumberjack.Logger{Filename: path, MaxSize: 10, MaxBackups: 5}, nil
}

// run applies d and keeps applying changes to it until ctx is done, then
// shuts down. stopSignals is called once shutdown starts so that a second
// interrupt gets the default behavior and kills the process.
func run(ctx context.Context, stopSignals func(), d *config.Dir, log *slog.Logger, setLevel func(string), rec *events.Recorder) {
	r := runner.New(log)
	r.Apply(d)
	logRunning(log, d)
	if d.Config.Web.Listen != "" {
		startWeb(ctx, d, r, rec, log)
	}

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

// startWeb serves the web UI until ctx is done. A UI that cannot start is
// logged and does not stop the stitches.
func startWeb(ctx context.Context, d *config.Dir, r *runner.Runner, rec *events.Recorder, log *slog.Logger) {
	srv, err := web.New(web.Options{ConfigDir: d.Path, Listen: d.Config.Web.Listen, Version: version, Runner: r, Events: rec, Log: log})
	if err != nil {
		log.Error("cannot start the web UI", "error", err)
		return
	}
	go func() {
		if err := srv.Serve(ctx); err != nil {
			log.Error("web UI stopped", "error", err)
		}
	}()
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
	if d.Config.Web != cur.Config.Web {
		log.Warn("web changes take effect after a restart", "listen", cur.Config.Web.Listen)
	}
	if c := r.Apply(d); c != (runner.Changes{}) {
		log.Info("configuration applied", "started", c.Started, "restarted", c.Restarted, "stopped", c.Stopped)
	} else {
		log.Debug("configuration reloaded; no stitch changed")
	}
	return d
}
