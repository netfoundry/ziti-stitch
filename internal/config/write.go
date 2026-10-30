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

package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/netfoundry/ziti-stitch/internal/atomicfile"
)

// dirPerm keeps the config directory private: identity files in it hold
// private keys.
const dirPerm = 0o700

// stitchesFilePerm: stitches.yaml holds no secrets.
const stitchesFilePerm = 0o644

// EnsureDir creates the config directory at path and its networks
// directory, if they do not exist, and makes sure only the owner (on
// Windows: SYSTEM and Administrators) can read them.
func EnsureDir(path string) error {
	if err := os.MkdirAll(filepath.Join(path, NetworksDirName), dirPerm); err != nil {
		return err
	}
	return securePrivateDir(path)
}

// IdentityPath returns where the identity file for network name lives.
func (d *Dir) IdentityPath(name string) string {
	return filepath.Join(d.Path, NetworksDirName, name+IdentityExt)
}

// StitchIndex returns the index of the stitch called name, or -1.
func (d *Dir) StitchIndex(name string) int {
	return slices.IndexFunc(d.Stitches, func(s Stitch) bool { return s.Name == name })
}

// StitchesUsing returns the names of the stitches, enabled or not, that
// bind or dial through the network called name.
func (d *Dir) StitchesUsing(name string) []string {
	var names []string
	for _, s := range d.Stitches {
		if s.From.Network == name || s.To.Network == name {
			names = append(names, s.Name)
		}
	}
	return names
}

// Edit applies change to d, validates the whole result and saves the
// stitches. Nothing is saved unless the result is valid, so a config that
// was already invalid can be repaired this way but never saved broken.
// d must have been read under Lock (Update does both), or a change saved
// since it was read is lost.
func (d *Dir) Edit(change func(d *Dir) error) error {
	if err := d.Editable(); err != nil {
		return err
	}
	if err := change(d); err != nil {
		return err
	}
	if err := d.Validate(); err != nil {
		return fmt.Errorf("not saved, the result would be invalid:\n%w", err)
	}
	return d.SaveStitches()
}

// Editable reports an error when the stitches are managed outside the
// config directory (EnvConfigSource), where ziti-stitch cannot change them.
func (d *Dir) Editable() error {
	if d.Source != "" {
		return fmt.Errorf("the stitches are managed in %s (%s), not here; change them there", d.Source, EnvConfigSource)
	}
	return nil
}

// SaveStitches writes d.Stitches to stitches.yaml. Values equal to their
// defaults are left out, so the file stays as short as a hand-written one.
// Comments in the existing file are not kept.
func (d *Dir) SaveStitches() error {
	if err := d.Editable(); err != nil {
		return err
	}
	sf := stitchesFile{Stitches: make([]Stitch, len(d.Stitches))}
	for i, s := range d.Stitches {
		sf.Stitches[i] = compact(s)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(sf); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(d.Path, StitchesFileName), buf.Bytes(), stitchesFilePerm)
}

// compact clears the fields of s that hold their default value; omitempty
// then leaves them out of the YAML.
func compact(s Stitch) Stitch {
	if s.AppData == AppDataPassthrough {
		s.AppData = ""
	}
	if s.From.Precedence == PrecedenceDefault {
		s.From.Precedence = ""
	}
	if s.To.ConnectTimeout == DefaultConnectTimeout {
		s.To.ConnectTimeout = 0
	}
	return s
}

// genericHostLabels say nothing about which network a controller belongs to.
var genericHostLabels = []string{"api", "client", "controller", "ctrl", "edge", "management", "mgmt", "openziti", "www", "zt", "ziti"}

// SuggestName proposes a network name from a controller URL: the first
// label of its host name that is neither generic ("ctrl", "api", ...) nor
// an id. https://ctrl.acme.example.com:1280 suggests "acme". It reports
// false when nothing usable is found, for instance for an IP address.
func SuggestName(controllerURL string) (string, bool) {
	u, err := url.Parse(controllerURL)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || net.ParseIP(host) != nil {
		return "", false
	}
	labels := strings.Split(host, ".")
	if len(labels) > 1 {
		labels = labels[:len(labels)-1] // the top-level domain names nothing
	}
	for _, l := range labels {
		if slices.Contains(genericHostLabels, l) || looksLikeID(l) {
			continue
		}
		if name := strings.TrimLeft(l, "-_"); CheckNewNetworkName(name) == nil {
			return name, true
		}
	}
	return "", false
}

// looksLikeID reports whether a host label is a generated id (a UUID, a
// hash) rather than a name a person chose.
func looksLikeID(label string) bool {
	if len(label) > 30 {
		return true
	}
	digits := 0
	for _, r := range label {
		if unicode.IsDigit(r) {
			digits++
		}
	}
	return len(label) >= 8 && digits*3 >= len(label)
}
