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

The UI is off until web.listen is set in config.yaml, e.g.:

  web:
    listen: 127.0.0.1:8474`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := readDir(*configDir)
			if err != nil {
				return err
			}
			if d.Config.Web.Listen == "" {
				return errors.New("the web UI is off: set web.listen in config.yaml (e.g. 127.0.0.1:8474) and restart ziti-stitch")
			}
			token, err := web.NewLoginToken(d.Path)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "http://%s/login?t=%s\nThe link works once, for five minutes.\n", d.Config.Web.Listen, token)
			return err
		},
	})
	return cmd
}
