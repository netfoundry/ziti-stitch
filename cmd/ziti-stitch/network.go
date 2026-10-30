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
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/netfoundry/ziti-stitch/internal/atomicfile"
	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/diagnose"
	"github.com/netfoundry/ziti-stitch/internal/network"
)

// readDir reads the config directory for a command that needs one to
// exist already.
func readDir(configDir string) (*config.Dir, error) {
	dir, err := resolveConfigDir(configDir)
	if err != nil {
		return nil, err
	}
	d, err := config.Read(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, noConfigDir(dir)
	}
	return d, err
}

// noConfigDir is the error for a command that needs a config directory
// when there is none at dir.
func noConfigDir(dir string) error {
	return fmt.Errorf("no config directory at %s; start with: ziti-stitch enroll --jwt <file>", dir)
}

// lockDir takes the config directory's edit lock, then reads it, for a
// command that changes the directory: nothing else can change it between
// the read and the write. create makes the directory if needed. The caller
// releases the lock with unlock.
func lockDir(configDir string, create bool) (d *config.Dir, unlock func(), err error) {
	dir, err := resolveConfigDir(configDir)
	if err != nil {
		return nil, nil, err
	}
	if create {
		if err := config.EnsureDir(dir); err != nil {
			return nil, nil, err
		}
	}
	unlock, err = config.Lock(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, noConfigDir(dir)
	}
	if err != nil {
		return nil, nil, err
	}
	if d, err = config.Read(dir); err != nil {
		unlock()
		return nil, nil, err
	}
	return d, unlock, nil
}

// createDir creates the config directory if needed and reads it.
func createDir(configDir string) (*config.Dir, error) {
	dir, err := resolveConfigDir(configDir)
	if err != nil {
		return nil, err
	}
	if err := config.EnsureDir(dir); err != nil {
		return nil, err
	}
	return config.Read(dir)
}

func newEnrollCmd(configDir *string) *cobra.Command {
	var jwtPath, name string
	cmd := &cobra.Command{
		Use:   "enroll --jwt <file|-> [--name <name>]",
		Short: "Enroll an identity from a one-time token and add its network",
		Long: `Enroll an identity from a one-time enrollment token (the JWT your network
admin created) and add its network to the config directory.

The network's name is how stitches refer to it. Without --name it is taken
from the controller's host name (ctrl.acme.example.com gives "acme").`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token, err := readToken(jwtPath, cmd.InOrStdin())
			if err != nil {
				return err
			}
			// A token works once: settle the name before redeeming it.
			controller, err := network.TokenController(token)
			if err != nil {
				return err
			}
			d, err := createDir(*configDir)
			if err != nil {
				return err
			}
			name, err := chooseName(d, name, controller)
			if err != nil {
				return err
			}
			warnSameController(cmd.ErrOrStderr(), d, []string{controller})

			raw, err := network.Enroll(token)
			if err != nil {
				return err
			}
			// Locked only now: redeeming can take minutes, and every other
			// change waits on the lock, the running instance's included.
			// So the name is checked again under it.
			d, unlock, err := lockDir(*configDir, false)
			if err != nil {
				return fmt.Errorf("enrolled, but saving the identity failed: %w", err)
			}
			defer unlock()
			if _, taken := d.Network(name); taken {
				return fmt.Errorf("network %q was added while this one enrolled, so the new identity was not saved; its token is spent, so ask for a new one", name)
			}
			if err := atomicfile.WriteFile(d.IdentityPath(name), raw, network.IdentityFilePerm); err != nil {
				return fmt.Errorf("enrolled, but saving the identity failed: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Enrolled network %q (controller %s).\nIdentity: %s\n", name, controller, d.IdentityPath(name))
			return err
		},
	}
	cmd.Flags().StringVar(&jwtPath, "jwt", "", "enrollment token file, or - for stdin (required)")
	cmd.Flags().StringVar(&name, "name", "", "network name (default: derived from the controller host)")
	_ = cmd.MarkFlagRequired("jwt")
	return cmd
}

func readToken(path string, stdin io.Reader) (string, error) {
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", errors.New("the enrollment token is empty")
	}
	return token, nil
}

// chooseName returns the network name to use: the given one, or one
// derived from the controller URL. It must be valid and not taken.
func chooseName(d *config.Dir, name, controller string) (string, error) {
	if name == "" {
		suggested, ok := config.SuggestName(controller)
		if !ok {
			return "", fmt.Errorf("cannot derive a network name from %s; pass --name", controller)
		}
		name = suggested
	}
	if !config.ValidName(name) {
		return "", fmt.Errorf("%q is not a valid network name: use %s", name, config.NameRule)
	}
	if _, taken := d.Network(name); taken {
		return "", fmt.Errorf("network %q already exists; pass --name to choose another", name)
	}
	return name, nil
}

// notSelfContained is the error for an identity file that refers to its
// key, certificate or CA elsewhere. ziti-stitch copies only the file, and
// renewal writes a key inline, which would quietly replace an HSM's.
func notSelfContained(path string) error {
	return fmt.Errorf("%s keeps its key, certificate or CA outside the file (a path, or an HSM); ziti-stitch needs them inline (pem:). Enroll a new identity with ziti-stitch enroll instead", path)
}

// warnSameController warns when an existing network already talks to one
// of these controllers. That can be deliberate (a second identity with
// different permissions) but is usually a mistake.
func warnSameController(w io.Writer, d *config.Dir, controllers []string) {
	hosts := map[string]bool{}
	for _, c := range controllers {
		hosts[hostOf(c)] = true
	}
	for _, n := range d.Networks {
		info, err := network.ReadInfo(n.IdentityFile)
		if err != nil {
			continue
		}
		for _, c := range info.Controllers {
			if hosts[hostOf(c)] {
				fmt.Fprintf(w, "warning: network %q already uses controller %s\n", n.Name, hostOf(c))
				return
			}
		}
	}
}

func hostOf(controller string) string {
	if u, err := url.Parse(controller); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return controller
}

func newNetworkCmd(configDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Manage networks (one enrolled identity each)",
	}
	cmd.AddCommand(
		newNetworkAddCmd(configDir),
		newNetworkListCmd(configDir),
		newNetworkRemoveCmd(configDir),
		newNetworkRenameCmd(configDir),
		newNetworkTestCmd(configDir),
	)
	return cmd
}

func newNetworkAddCmd(configDir *string) *cobra.Command {
	var identityPath, name string
	cmd := &cobra.Command{
		Use:   "add --identity <file> [--name <name>]",
		Short: "Add a network from an identity that is already enrolled",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := os.ReadFile(identityPath)
			if err != nil {
				return err
			}
			info, err := network.ReadInfo(identityPath)
			if err != nil {
				return fmt.Errorf("%s: %w", identityPath, err)
			}
			if !info.SelfContained {
				return notSelfContained(identityPath)
			}
			d, unlock, err := lockDir(*configDir, true)
			if err != nil {
				return err
			}
			defer unlock()
			name, err := chooseName(d, name, info.Controllers[0])
			if err != nil {
				return err
			}
			warnSameController(cmd.ErrOrStderr(), d, info.Controllers)
			if err := atomicfile.WriteFile(d.IdentityPath(name), raw, network.IdentityFilePerm); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Added network %q (controller %s).\nIdentity copied to %s; the original can be deleted.\n", name, info.Controllers[0], d.IdentityPath(name))
			return err
		},
	}
	cmd.Flags().StringVar(&identityPath, "identity", "", "enrolled identity file (required)")
	cmd.Flags().StringVar(&name, "name", "", "network name (default: derived from the controller host)")
	_ = cmd.MarkFlagRequired("identity")
	return cmd
}

func newNetworkListCmd(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List networks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := readDir(*configDir)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tCONTROLLER\tCERTIFICATE EXPIRES\tSTITCHES")
			for _, n := range d.Networks {
				controller, expires := "?", "?"
				if info, err := network.ReadInfo(n.IdentityFile); err == nil {
					controller = hostOf(info.Controllers[0])
					expires = network.DescribeExpiry(info.CertNotAfter, time.Now())
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n.Name, controller, expires, orDash(strings.Join(d.StitchesUsing(n.Name), ", ")))
			}
			return tw.Flush()
		},
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newNetworkRemoveCmd(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a network and delete its identity file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			d, unlock, err := lockDir(*configDir, false)
			if err != nil {
				return err
			}
			defer unlock()
			n, ok := d.Network(name)
			if !ok {
				return fmt.Errorf("no network %q", name)
			}
			if users := d.StitchesUsing(name); len(users) > 0 {
				return fmt.Errorf("network %q is used by stitches %s; remove those first", name, strings.Join(users, ", "))
			}
			if err := checkNotWebNetwork(d, name); err != nil {
				return err
			}
			if err := os.Remove(n.IdentityFile); err != nil {
				return err
			}
			if err := os.Remove(n.IdentityFile + network.PendingSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Removed network %q.\nThe identity still exists on its controller; ask the network admin to delete it if it is no longer needed.\n", name)
			return err
		},
	}
}

func newNetworkRenameCmd(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "Rename a network, updating the stitches that use it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			from, to := args[0], args[1]
			d, unlock, err := lockDir(*configDir, false)
			if err != nil {
				return err
			}
			defer unlock()
			old, ok := d.Network(from)
			if !ok {
				return fmt.Errorf("no network %q", from)
			}
			if !config.ValidName(to) {
				return fmt.Errorf("%q is not a valid network name: use %s", to, config.NameRule)
			}
			if _, taken := d.Network(to); taken {
				return fmt.Errorf("network %q already exists", to)
			}
			if err := checkNotWebNetwork(d, from); err != nil {
				return err
			}
			// Renaming rewrites the stitches, which may be managed elsewhere.
			if err := d.Editable(); err != nil {
				return err
			}
			if _, err := os.Stat(old.IdentityFile + network.PendingSuffix); err == nil {
				return fmt.Errorf("network %q is in the middle of a certificate renewal; try again once ziti-stitch has finished it", from)
			}

			for i := range d.Stitches {
				s := &d.Stitches[i]
				if s.From.Network == from {
					s.From.Network = to
				}
				if s.To.Network == from {
					s.To.Network = to
				}
			}
			newPath := d.IdentityPath(to)
			if err := atomicfile.Rename(old.IdentityFile, newPath); err != nil {
				return err
			}
			if err := d.SaveStitches(); err != nil {
				if rbErr := atomicfile.Rename(newPath, old.IdentityFile); rbErr != nil {
					return fmt.Errorf("saving stitches: %w (and putting the identity file back failed: %v)", err, rbErr)
				}
				return fmt.Errorf("saving stitches: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Renamed network %q to %q.\n", from, to)
			return err
		},
	}
}

func newNetworkTestCmd(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "test <name>",
		Short: "Authenticate a network's identity and check what it can see",
		Long: `Authenticate a network's identity, list the services it can see with its
Dial and Bind permissions, and check the stitches that use the network.

Problems name the policy the network admin needs to add.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			d, err := readDir(*configDir)
			if err != nil {
				return err
			}
			n, ok := d.Network(name)
			if !ok {
				return fmt.Errorf("no network %q", name)
			}
			out := cmd.OutOrStdout()
			if info, err := network.ReadInfo(n.IdentityFile); err == nil {
				fmt.Fprintf(out, "Controller:   %s\nCertificate:  expires %s\n", info.Controllers[0], network.DescribeExpiry(info.CertNotAfter, time.Now()))
			}
			r := diagnose.Probe(n.Name, n.IdentityFile)
			if r.Err != nil {
				return fmt.Errorf("cannot authenticate: %w", r.Err)
			}
			fmt.Fprintf(out, "Identity:     %s\n\n", r.IdentityName)

			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVICE\tDIAL\tBIND")
			for _, svc := range slices.Sorted(maps.Keys(r.Services)) {
				a := r.Services[svc]
				fmt.Fprintf(tw, "%s\t%s\t%s\n", svc, yesNo(a.Dial), yesNo(a.Bind))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if len(r.Services) == 0 {
				fmt.Fprintln(out, "(no services visible to this identity)")
			}

			findings := diagnose.Stitches(d.Stitches, map[string]diagnose.NetworkReport{n.Name: r})
			if len(findings) > 0 {
				fmt.Fprintln(out)
			}
			return problemsErr(printFindings(out, findings))
		},
	}
}

// checkNotWebNetwork refuses to touch the network the web UI binds
// through. config.yaml is the user's file, with their comments, so the CLI
// does not rewrite it to follow along.
func checkNotWebNetwork(d *config.Dir, name string) error {
	if z := d.Config.Web.Ziti; z != nil && z.Network == name {
		return fmt.Errorf("the web UI is served over network %q (web.ziti.network in %s); change that first", name, config.ConfigFileName)
	}
	return nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}
