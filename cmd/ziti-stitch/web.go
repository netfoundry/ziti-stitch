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
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/netfoundry/ziti-stitch/internal/web"
)

func newWebCmd(configDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Sign in to the web UI",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "login",
		Short: "Print a one-time link that signs you in to the web UI",
		Long: `Print a one-time link that signs you in to the web UI, valid for five
minutes. Run it as a user who can change the configuration (sudo, if it
belongs to root): being able to sign in is the same as being able to edit
the config directory.

The UI is off until web.listen or web.ziti is set in config.yaml, e.g.:

  web:
    listen: 127.0.0.1:8474

A link works on either listener. Over Ziti, allowed identities need no link.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := readDir(*configDir)
			if err != nil {
				return err
			}
			w := d.Config.Web
			if w.Listen == "" && w.Ziti == nil {
				return errors.New("the web UI is off: set web.listen (e.g. 127.0.0.1:8474) or web.ziti in config.yaml and restart ziti-stitch")
			}
			// The config is not validated here, and web.ziti without hosts
			// is invalid.
			var zitiHost string
			if w.Ziti != nil && len(w.Ziti.Hosts) > 0 {
				zitiHost = w.Ziti.Hosts[0]
			}
			if w.Listen == "" && zitiHost == "" {
				return errors.New("web.ziti in config.yaml has no hosts, so there is no address to link to; run ziti-stitch validate")
			}
			token, err := web.NewLoginToken(d.Path)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if w.Listen != "" {
				fmt.Fprintf(out, "http://%s/login?t=%s\n", w.Listen, token)
			}
			if zitiHost != "" {
				fmt.Fprintf(out, "http://%s/login?t=%s   (over Ziti)\n", zitiHost, token)
			}
			_, err = fmt.Fprintln(out, "The link works once, for five minutes.")
			return err
		},
	})
	return cmd
}
