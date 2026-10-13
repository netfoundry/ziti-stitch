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
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/diagnose"
	"github.com/netfoundry/ziti-stitch/internal/network"
)

// certWarning is how far ahead validate warns about a certificate that has
// not renewed. ziti-stitch renews 30 days ahead while running, so a
// certificate this close to expiry means it has not been.
const certWarning = 14 * 24 * time.Hour

func newValidateCmd(configDir *string) *cobra.Command {
	var offline bool
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check the configuration and what each identity is allowed to do",
		Long: `Check the configuration, then authenticate every network used by an enabled
stitch and confirm that its identity has Bind on the services it binds and
Dial on the services it dials. Problems name the policy that is missing.

--offline checks only the configuration and certificate expiry.`,
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
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Configuration in %s is valid.\n", dir)

			used := usedNetworks(d)
			problems := 0
			now := time.Now()
			for _, n := range used {
				info, err := network.ReadInfo(n.IdentityFile)
				switch {
				case err != nil:
					problems++
					fmt.Fprintf(out, "problem  network %s: %v\n", n.Name, err)
				case !info.CertNotAfter.IsZero() && info.CertNotAfter.Sub(now) < certWarning:
					problems++
					fmt.Fprintf(out, "problem  network %s: certificate expires %s\n", n.Name, describeExpiry(info.CertNotAfter, now))
				}
			}
			if offline {
				return problemsErr(problems)
			}

			reports := map[string]diagnose.NetworkReport{}
			for _, n := range used {
				reports[n.Name] = diagnose.Probe(n.Name, n.IdentityFile)
			}
			problems += printFindings(out, diagnose.Stitches(d.Stitches, reports))
			return problemsErr(problems)
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "skip the checks that contact the networks")
	return cmd
}

// usedNetworks returns the networks enabled stitches use, in name order.
func usedNetworks(d *config.Dir) []config.Network {
	used := map[string]bool{}
	for _, s := range d.Stitches {
		if !s.Disabled {
			used[s.From.Network] = true
			used[s.To.Network] = true
		}
	}
	var nets []config.Network
	for _, n := range d.Networks {
		if used[n.Name] {
			nets = append(nets, n)
		}
	}
	return nets
}

// printFindings prints findings and returns how many are problems.
func printFindings(w io.Writer, findings []diagnose.Finding) int {
	problems := 0
	for _, f := range findings {
		label := "ok      "
		if f.Severity == diagnose.Problem {
			label = "problem "
			problems++
		}
		fmt.Fprintf(w, "%s stitch %s: %s\n", label, f.Stitch, f.Message)
	}
	return problems
}

// problemsErr makes the command fail when n > 0; the problems themselves
// have been printed already.
func problemsErr(n int) error {
	switch n {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("1 problem found")
	default:
		return fmt.Errorf("%d problems found", n)
	}
}
