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
	"os"
	"path/filepath"
	"strings"

	"github.com/netfoundry/ziti-stitch/internal/atomicfile"
	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/network"
)

// enrollFrom adds the networks named by the files in dir that d does not
// have yet: <network>.jwt is enrolled, <network>.json (an identity already
// enrolled) is copied. With prefix set, only <prefix>.<network>.jwt and
// .json count, so one directory can hold every instance's. Networks d has
// are left alone, so it is safe on every start: a renewed certificate is
// never replaced by the original. A missing dir is not an error; a token
// that cannot be redeemed is.
//
// It warns about the .json files it did not use (another instance's, or
// one for a network d already has): they hold private keys nothing here
// needs, which should be removed.
//
// It is for Kubernetes, where a Secret holds every pod's tokens, prefixed
// with the pod's name; Secret volumes also hold hidden entries (..data),
// which are skipped.
func enrollFrom(dir, prefix string, d *config.Dir, out io.Writer) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	added := map[string]bool{}
	var unused []string // .json files, which hold private keys
	noteUnused := func(file, ext string) {
		if ext == ".json" {
			unused = append(unused, file)
		}
	}
	for _, e := range entries {
		file := e.Name()
		ext := filepath.Ext(file)
		if strings.HasPrefix(file, ".") || (ext != ".jwt" && ext != ".json") {
			continue
		}
		name := strings.TrimSuffix(file, ext)
		if prefix != "" {
			var ours bool
			if name, ours = strings.CutPrefix(name, prefix+"."); !ours {
				noteUnused(file, ext)
				continue
			}
		}
		path := filepath.Join(dir, file)
		if !config.ValidName(name) {
			return fmt.Errorf("%s: %q is not a valid network name (%s)", path, name, config.NameRule)
		}
		if _, ok := d.Network(name); ok || added[name] {
			noteUnused(file, ext)
			continue
		}
		if err := config.CheckNewNetworkName(name); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		var raw []byte
		if ext == ".jwt" {
			token, err := readToken(path, nil)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if raw, err = network.Enroll(token); err != nil {
				return fmt.Errorf("enrolling network %q from %s: %w", name, path, err)
			}
		} else {
			info, err := network.ReadInfo(path)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if !info.SelfContained {
				return notSelfContained(path)
			}
			if raw, err = os.ReadFile(path); err != nil {
				return err
			}
		}
		if err := atomicfile.WriteFile(d.IdentityPath(name), raw, network.IdentityFilePerm); err != nil {
			return fmt.Errorf("saving the identity for network %q: %w", name, err)
		}
		added[name] = true
		verb := "Enrolled"
		if ext == ".json" {
			verb = "Added"
		}
		fmt.Fprintf(out, "%s network %q from %s.\n", verb, name, path)
	}
	if len(unused) > 0 {
		fmt.Fprintf(out, "warning: %s holds enrolled identities this instance does not use (%s). They contain private keys: remove them once every instance has started.\n", dir, strings.Join(unused, ", "))
	}
	return nil
}
