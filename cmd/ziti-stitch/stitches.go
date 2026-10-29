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
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

// savedNote tells the user how a saved change reaches a running instance.
const savedNote = "A running ziti-stitch applies the change on its own."

// stoppedNote says what becomes of a removed or disabled stitch's open
// connections.
const stoppedNote = "Its open connections get 30 seconds to finish, then are closed."

func newStitchCmd(configDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stitch",
		Short: "Manage stitches",
	}
	cmd.AddCommand(
		newStitchAddCmd(configDir),
		newStitchListCmd(configDir),
		newStitchRemoveCmd(configDir),
		newStitchSetEnabledCmd(configDir, true),
		newStitchSetEnabledCmd(configDir, false),
	)
	return cmd
}

// editStitches reads the config under its edit lock and applies change
// through Dir.Edit.
func editStitches(configDir string, change func(d *config.Dir) error) error {
	d, unlock, err := lockDir(configDir, false)
	if err != nil {
		return err
	}
	defer unlock()
	return d.Edit(change)
}

// parseEndpoint splits "network:service". Network names cannot contain
// ':', so everything after the first one is the service name.
func parseEndpoint(s string) (network, service string, err error) {
	network, service, ok := strings.Cut(s, ":")
	if !ok || network == "" || service == "" {
		return "", "", fmt.Errorf("%q is not network:service", s)
	}
	return network, service, nil
}

func newStitchAddCmd(configDir *string) *cobra.Command {
	var (
		from, to, appData, precedence string
		cost                          uint16
		connectTimeout, idleTimeout   time.Duration
		maxConnections                int
		disabled                      bool
	)
	cmd := &cobra.Command{
		Use:   "add <name> --from <network:service> --to <network:service>",
		Short: "Add a stitch",
		Long: `Add a stitch: bind the --from service and, for every connection, dial the
--to service.

App data (what the dialer sent along, e.g. a tunneler's intercepted address):
  passthrough  forward it unchanged (default)
  strip        send none
  annotate     forward it with a ziti_stitch object added, naming the stitch
               and the original dialer. Reveals ingress identity names to the
               egress network. Dials whose app data is not a JSON object
               are refused.

Only annotate writes the ziti_stitch object; passthrough forwards whatever
the dialer sent, including one it made up. The egress side should trust
that object only from a stitch it knows annotates.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			s := config.Stitch{
				Name:           name,
				AppData:        config.AppDataPolicy(appData),
				IdleTimeout:    idleTimeout,
				MaxConnections: maxConnections,
				Disabled:       disabled,
			}
			var err error
			if s.From.Network, s.From.Service, err = parseEndpoint(from); err != nil {
				return fmt.Errorf("--from: %w", err)
			}
			if s.To.Network, s.To.Service, err = parseEndpoint(to); err != nil {
				return fmt.Errorf("--to: %w", err)
			}
			s.From.Precedence, s.From.Cost = precedence, cost
			s.To.ConnectTimeout = connectTimeout

			if err := editStitches(*configDir, func(d *config.Dir) error {
				if d.StitchIndex(name) >= 0 {
					return fmt.Errorf("stitch %q already exists", name)
				}
				d.Stitches = append(d.Stitches, s)
				return nil
			}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Added stitch %q: %s -> %s.\n%s\n", name, from, to, savedNote)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&from, "from", "", "network:service to bind (required)")
	f.StringVar(&to, "to", "", "network:service to dial (required)")
	f.StringVar(&appData, "app-data", string(config.AppDataPassthrough), "passthrough, strip or annotate")
	f.StringVar(&precedence, "precedence", config.PrecedenceDefault, "terminator precedence: default, required or failed (to prefer one of several instances, use --cost; see docs/high-availability.md)")
	f.Uint16Var(&cost, "cost", 0, "terminator cost: with several instances, the lowest gets the dials while healthy")
	f.DurationVar(&connectTimeout, "connect-timeout", config.DefaultConnectTimeout, "how long to wait for the egress dial")
	f.DurationVar(&idleTimeout, "idle-timeout", 0, "close connections idle this long (0 never)")
	f.IntVar(&maxConnections, "max-connections", 0, "refuse connections beyond this many open at once (0 no limit)")
	f.BoolVar(&disabled, "disabled", false, "add the stitch disabled")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func newStitchListCmd(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List stitches",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := readDir(*configDir)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tFROM\tTO\tAPP DATA\tSTATUS")
			for _, s := range d.Stitches {
				status := "enabled"
				if s.Disabled {
					status = "disabled"
				}
				fmt.Fprintf(tw, "%s\t%s:%s\t%s:%s\t%s\t%s\n", s.Name, s.From.Network, s.From.Service, s.To.Network, s.To.Service, s.AppData, status)
			}
			return tw.Flush()
		},
	}
}

func newStitchRemoveCmd(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a stitch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := editStitches(*configDir, func(d *config.Dir) error {
				i := d.StitchIndex(name)
				if i < 0 {
					return fmt.Errorf("no stitch %q", name)
				}
				d.Stitches = slices.Delete(d.Stitches, i, i+1)
				return nil
			}); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Removed stitch %q. %s\n%s\n", name, stoppedNote, savedNote)
			return err
		},
	}
}

func newStitchSetEnabledCmd(configDir *string, enable bool) *cobra.Command {
	use, short, done := "disable <name>", "Stop a stitch without removing it", "Disabled"
	if enable {
		use, short, done = "enable <name>", "Start a disabled stitch", "Enabled"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := editStitches(*configDir, func(d *config.Dir) error {
				i := d.StitchIndex(name)
				if i < 0 {
					return fmt.Errorf("no stitch %q", name)
				}
				d.Stitches[i].Disabled = !enable
				return nil
			}); err != nil {
				return err
			}
			msg := fmt.Sprintf("%s stitch %q.", done, name)
			if !enable {
				msg += " " + stoppedNote
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\n%s\n", msg, savedNote)
			return err
		},
	}
}
