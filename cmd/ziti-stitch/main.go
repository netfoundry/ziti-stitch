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

// Command ziti-stitch joins services across independent OpenZiti networks.
package main

import (
	"fmt"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ziti-stitch",
		Short: "Stitch services across independent OpenZiti networks",
		// Usage on every runtime error buries the error itself; cobra
		// still prints usage for bad flags and unknown commands.
		SilenceUsage:  true,
		SilenceErrors: true,
		// The SDK logs at info for routine things (key generation, router
		// connections); in a CLI command that is noise between the output
		// lines. run replaces this with its own logging setup.
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			logrus.SetOutput(cmd.ErrOrStderr())
			logrus.SetLevel(logrus.WarnLevel)
			logrus.SetFormatter(newSDKFormatter(&logrus.TextFormatter{DisableTimestamp: true}))
		},
	}
	var configDir string
	root.PersistentFlags().StringVar(&configDir, "config-dir", "",
		"config directory (default $"+config.EnvConfigDir+", else a per-OS location)")
	root.AddCommand(
		newRunCmd(&configDir),
		newEnrollCmd(&configDir),
		newNetworkCmd(&configDir),
		newStitchCmd(&configDir),
		newValidateCmd(&configDir),
		newVersionCmd(),
	)
	return root
}

// resolveConfigDir returns the --config-dir value, or the default.
func resolveConfigDir(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	return config.DefaultDir()
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version)
			return err
		},
	}
}
