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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

const serviceName = "ziti-stitch"

// macLogDir is where the launchd service writes its log, under
// /Library/Logs so that Console.app shows it.
const macLogDir = "/Library/Logs/ziti-stitch"

// serviceConfig describes the system service for a config directory (an
// absolute path). userName runs it as that user on Linux and macOS.
func serviceConfig(configDir, userName string) (*service.Config, error) {
	cfg := &service.Config{
		Name:        serviceName,
		DisplayName: "Ziti Stitch",
		Description: "Stitches services across independent OpenZiti networks",
		UserName:    userName,
		Arguments:   []string{"run", "--config-dir", configDir},
		Option:      service.KeyValue{},
	}
	switch runtime.GOOS {
	case "linux":
		if err := checkUnitConfigDir(configDir); err != nil {
			return nil, err
		}
		cfg.Option["SystemdScript"] = systemdUnit(configDir)
		// journald collects stderr; no log file.
	case "darwin":
		cfg.Arguments = append(cfg.Arguments, "--log-file", filepath.Join(macLogDir, serviceName+".log"))
		cfg.Option["KeepAlive"] = true
		cfg.Option["RunAtLoad"] = true
		// launchd's own capture of stdout/stderr, for a crash's last words.
		cfg.Option["LogOutput"] = true
		cfg.Option["LogDirectory"] = macLogDir
	case "windows":
		if userName != "" {
			return nil, errors.New("--user is not supported on Windows; the service runs as LocalSystem")
		}
		cfg.Arguments = append(cfg.Arguments, "--log-file", windowsLogFile(configDir))
		cfg.Option["OnFailure"] = "restart"
		cfg.Option["OnFailureDelayDuration"] = "5s"
	}
	return cfg, nil
}

func windowsLogFile(configDir string) string {
	return filepath.Join(configDir, "logs", serviceName+".log")
}

// program adapts run to a service manager's start and stop calls. Only the
// Windows service control manager needs this; elsewhere `run` handles
// signals itself.
type program struct {
	run    func(ctx context.Context)
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *program) Start(service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	go func() {
		defer close(p.done)
		p.run(ctx)
	}()
	return nil
}

func (p *program) Stop(service.Service) error {
	p.cancel()
	<-p.done
	return nil
}

// controlOnly is the service.Interface for commands that only install or
// control the service; they never run it.
type controlOnly struct{}

func (controlOnly) Start(service.Service) error { return nil }
func (controlOnly) Stop(service.Service) error  { return nil }

func newServiceCmd(configDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install and control ziti-stitch as a system service",
		Long: `Install and control ziti-stitch as a system service: systemd on Linux,
launchd on macOS, the service control manager on Windows. These commands
need root (sudo) or an Administrator prompt.

The service runs "ziti-stitch run" with the config directory given here
(--config-dir, else the system default) from the binary's current location;
install the binary somewhere permanent, such as /usr/local/bin, first.`,
	}
	cmd.AddCommand(newServiceInstallCmd(configDir))
	for _, action := range []struct{ use, short, done string }{
		{"uninstall", "Remove the service (the config directory is kept)", "removed"},
		{"start", "Start the service", "started"},
		{"stop", "Stop the service", "stopped"},
		{"restart", "Restart the service", "restarted"},
	} {
		cmd.AddCommand(&cobra.Command{
			Use:   action.use,
			Short: action.short,
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				s, err := newControlService()
				if err != nil {
					return err
				}
				if err := service.Control(s, action.use); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Service %s %s.\n", serviceName, action.done)
				return err
			},
		})
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show whether the service is installed and running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newControlService()
			if err != nil {
				return err
			}
			st, err := s.Status()
			state := "unknown"
			switch {
			case errors.Is(err, service.ErrNotInstalled):
				state = "not installed"
			case err != nil:
				return err
			case st == service.StatusRunning:
				state = "running"
			case st == service.StatusStopped:
				state = "stopped"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", serviceName, state)
			return err
		},
	})
	return cmd
}

// newControlService returns a handle for controlling the installed service.
// Only the name matters for that.
func newControlService() (service.Service, error) {
	return service.New(controlOnly{}, &service.Config{Name: serviceName})
}

func newServiceInstallCmd(configDir *string) *cobra.Command {
	var userName string
	cmd := &cobra.Command{
		Use:   "install [--user <name>]",
		Short: "Install the service, set to start at boot",
		Long: `Install the service, set to start at boot, running from this binary's
location with the config directory given by --config-dir (default: the
system location). The directory is created if needed, readable only by
root (SYSTEM and Administrators on Windows).

Logs: journalctl -u ziti-stitch on Linux; ` + macLogDir + `/ on macOS;
<config dir>\logs\ on Windows.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "windows" && os.Geteuid() != 0 {
				return errors.New("installing a system service needs root; run it with sudo")
			}
			dir, err := resolveConfigDir(*configDir)
			if err != nil {
				return err
			}
			if dir, err = filepath.Abs(dir); err != nil {
				return err
			}
			cfg, err := serviceConfig(dir, userName)
			if err != nil {
				return err
			}
			if err := config.EnsureDir(dir); err != nil {
				return err
			}
			if userName != "" {
				if err := checkOwnedBy(dir, userName); err != nil {
					return err
				}
			}
			if runtime.GOOS == "darwin" {
				if err := os.MkdirAll(macLogDir, 0o750); err != nil {
					return err
				}
				// The service writes its log there as that user.
				if userName != "" {
					if err := chownTo(macLogDir, userName); err != nil {
						return err
					}
				}
			}
			warnAboutInstall(cmd.ErrOrStderr(), dir)

			s, err := service.New(controlOnly{}, cfg)
			if err != nil {
				return err
			}
			if err := s.Install(); err != nil {
				return fmt.Errorf("installing the service: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Installed service %s (%s) with config directory %s.\nStart it with: ziti-stitch service start\n", serviceName, s.Platform(), dir)
			return err
		},
	}
	cmd.Flags().StringVar(&userName, "user", "", "run the service as this user (Linux, macOS); it must own the config directory")
	return cmd
}

// warnAboutInstall points out things that will bite after installing: a
// binary in a place that may not last, and a config with nothing to run.
func warnAboutInstall(w io.Writer, dir string) {
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		home, _ := os.UserHomeDir()
		for _, risky := range []string{os.TempDir(), home} {
			if risky != "" && strings.HasPrefix(exe, risky+string(filepath.Separator)) {
				fmt.Fprintf(w, "warning: the service will run %s; consider installing the binary somewhere permanent (e.g. /usr/local/bin) first\n", exe)
				break
			}
		}
	}
	d, err := config.Load(dir)
	switch {
	case err != nil:
		fmt.Fprintf(w, "warning: the configuration is not valid yet; the service will wait until it is:\n%v\n", err)
	case len(d.Stitches) == 0:
		fmt.Fprintln(w, "note: no stitches configured yet; the service picks them up when you add them")
	}
}
