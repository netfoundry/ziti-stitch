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

// Package config loads and validates a ziti-stitch config directory:
//
//	config.yaml          global settings (optional)
//	stitches.yaml        the stitches (optional)
//	networks/<name>.json one enrolled Ziti identity per network
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	ConfigFileName   = "config.yaml"
	StitchesFileName = "stitches.yaml"
	NetworksDirName  = "networks"
	IdentityExt      = ".json"

	DefaultConnectTimeout = 5 * time.Second
)

// AppDataPolicy says what a stitch does with the app data the ingress
// dialer sent.
type AppDataPolicy string

const (
	// AppDataPassthrough forwards the ingress app data byte for byte.
	AppDataPassthrough AppDataPolicy = "passthrough"
	// AppDataStrip sends no app data on the egress dial.
	AppDataStrip AppDataPolicy = "strip"
	// AppDataAnnotate forwards the app data with a ziti_stitch object added.
	AppDataAnnotate AppDataPolicy = "annotate"
)

// Precedence labels, as the Ziti SDK spells them.
const (
	PrecedenceDefault  = "default"
	PrecedenceRequired = "required"
	PrecedenceFailed   = "failed"
)

// Config is the content of config.yaml.
type Config struct {
	Log Log `yaml:"log"`
	Web Web `yaml:"web"`
}

// Web configures the web UI. Either listener, both or neither may be on.
type Web struct {
	// Listen is a loopback host:port to serve the UI on; empty turns it off.
	Listen string   `yaml:"listen,omitempty"`
	Ziti   *WebZiti `yaml:"ziti,omitempty"`
}

// WebZiti serves the UI as a Ziti service, bound by one of our networks'
// identities.
type WebZiti struct {
	Network string `yaml:"network"`
	Service string `yaml:"service"`
	// Hosts are the names people reach the service by (its intercept
	// addresses), each with the intercept port if it is not 80; requests
	// naming any other host are refused, which is what stops DNS
	// rebinding from turning a browser against the UI.
	Hosts []string `yaml:"hosts"`
	// Allow lists dialer identity ids with full access.
	Allow []string `yaml:"allow,omitempty"`
	// PolicyAccess is what any identity the service policy lets dial gets:
	// none (the default), read or edit.
	PolicyAccess string `yaml:"policy_access,omitempty"`
}

// Access levels for WebZiti.PolicyAccess.
const (
	AccessNone = "none"
	AccessRead = "read"
	AccessEdit = "edit"
)

// SameListeners reports whether w and o serve the UI the same way: the
// same loopback address and the same Ziti service and hosts. Who may use
// it (Allow, PolicyAccess) is not part of that; it applies without a
// restart.
func (w Web) SameListeners(o Web) bool {
	if w.Listen != o.Listen || (w.Ziti == nil) != (o.Ziti == nil) {
		return false
	}
	if w.Ziti == nil {
		return true
	}
	return w.Ziti.Network == o.Ziti.Network && w.Ziti.Service == o.Ziti.Service && slices.Equal(w.Ziti.Hosts, o.Ziti.Hosts)
}

// Log configures ziti-stitch's own logging.
type Log struct {
	Level  string `yaml:"level"`  // debug, info, warn or error
	Format string `yaml:"format"` // text or json
}

// Stitch binds a service on one network and dials a service on another
// for every connection that arrives.
type Stitch struct {
	Name        string        `yaml:"name"`
	From        Ingress       `yaml:"from"`
	To          Egress        `yaml:"to"`
	AppData     AppDataPolicy `yaml:"app_data,omitempty"`
	IdleTimeout time.Duration `yaml:"idle_timeout,omitempty"` // 0 disables it
	Disabled    bool          `yaml:"disabled,omitempty"`
}

// Ingress is the service a stitch binds.
type Ingress struct {
	Network    string `yaml:"network"`
	Service    string `yaml:"service"`
	Precedence string `yaml:"precedence,omitempty"`
	Cost       uint16 `yaml:"cost,omitempty"`
}

// Egress is the service a stitch dials.
type Egress struct {
	Network        string        `yaml:"network"`
	Service        string        `yaml:"service"`
	ConnectTimeout time.Duration `yaml:"connect_timeout,omitempty"`
}

// Network is an enrolled identity in the networks directory. Its name is
// the file name without the extension.
type Network struct {
	Name         string
	IdentityFile string
}

// Dir is a loaded, defaulted and validated config directory.
type Dir struct {
	Path     string
	Config   Config
	Networks []Network // sorted by name
	Stitches []Stitch
}

type stitchesFile struct {
	Stitches []Stitch `yaml:"stitches"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidName reports whether s can name a network or a stitch: lower case
// letters, digits, '-' and '_', starting with a letter or digit, at most
// 63 characters. Names end up in file names, logs and URLs.
func ValidName(s string) bool {
	return nameRE.MatchString(s)
}

// Load reads the config directory at path, fills in defaults and validates
// the result. Validation problems are reported together.
func Load(path string) (*Dir, error) {
	d, err := Read(path)
	if err != nil {
		return nil, err
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return d, nil
}

// Read reads the config directory at path and fills in defaults, without
// validating the stitches. Commands that change the config use it, so that
// a config that is already broken can still be fixed; they validate the
// result before saving it.
func Read(path string) (*Dir, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", path)
	}

	d := &Dir{Path: path}
	if err := decodeYAML(filepath.Join(path, ConfigFileName), &d.Config); err != nil {
		return nil, err
	}
	var sf stitchesFile
	if err := decodeYAML(filepath.Join(path, StitchesFileName), &sf); err != nil {
		return nil, err
	}
	d.Stitches = sf.Stitches
	if d.Networks, err = listNetworks(filepath.Join(path, NetworksDirName)); err != nil {
		return nil, err
	}
	d.applyDefaults()
	return d, nil
}

// Network returns the network with the given name.
func (d *Dir) Network(name string) (Network, bool) {
	i := slices.IndexFunc(d.Networks, func(n Network) bool { return n.Name == name })
	if i < 0 {
		return Network{}, false
	}
	return d.Networks[i], true
}

// decodeYAML decodes the file at path into v, rejecting unknown fields so
// that a typo is an error instead of a silently ignored setting. A missing
// or empty file leaves v untouched.
func decodeYAML(path string, v any) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func listNetworks(dir string) ([]Network, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var nets []Network
	for _, e := range entries {
		n := e.Name()
		// Dot files are atomicfile temporaries; *.json.pending files are
		// identities mid-renewal, which the network package resolves.
		if e.IsDir() || strings.HasPrefix(n, ".") || !strings.HasSuffix(n, IdentityExt) {
			continue
		}
		name := strings.TrimSuffix(n, IdentityExt)
		if !ValidName(name) {
			return nil, fmt.Errorf("%s: %q is not a valid network name (lower case letters, digits, '-' and '_')", filepath.Join(dir, n), name)
		}
		nets = append(nets, Network{Name: name, IdentityFile: filepath.Join(dir, n)})
	}
	// os.ReadDir sorts by file name, so nets is sorted by name.
	return nets, nil
}

func (d *Dir) applyDefaults() {
	if d.Config.Log.Level == "" {
		d.Config.Log.Level = "info"
	}
	if d.Config.Log.Format == "" {
		d.Config.Log.Format = "text"
	}
	if z := d.Config.Web.Ziti; z != nil && z.PolicyAccess == "" {
		z.PolicyAccess = AccessNone
	}
	for i, s := range d.Stitches {
		d.Stitches[i] = s.WithDefaults()
	}
}

// WithDefaults returns s with defaults in place of unset values.
func (s Stitch) WithDefaults() Stitch {
	if s.AppData == "" {
		s.AppData = AppDataPassthrough
	}
	if s.From.Precedence == "" {
		s.From.Precedence = PrecedenceDefault
	}
	if s.To.ConnectTimeout == 0 {
		s.To.ConnectTimeout = DefaultConnectTimeout
	}
	return s
}

// Validate checks the whole configuration and reports every problem.
func (d *Dir) Validate() error {
	var p problems
	d.validateLog(&p)
	d.validateWeb(&p)
	ingress := d.validateStitches(&p)
	if z := d.Config.Web.Ziti; z != nil {
		// One identity binding a service twice splits its dials between
		// the UI and the stitch.
		key := endpointKey(z.Network, z.Service)
		if other, ok := ingress[key]; ok {
			p.add("%s: web.ziti binds %s, which stitch %q also binds", ConfigFileName, key, other)
		}
	}
	if len(p) == 0 {
		p = append(p, d.findCycles()...)
	}
	return errors.Join(p...)
}

// problems collects what Validate finds, so that it reports all of it.
type problems []error

func (p *problems) add(format string, args ...any) {
	*p = append(*p, fmt.Errorf(format, args...))
}

func (d *Dir) validateLog(p *problems) {
	switch d.Config.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		p.add("%s: log.level %q is not one of debug, info, warn, error", ConfigFileName, d.Config.Log.Level)
	}
	switch d.Config.Log.Format {
	case "text", "json":
	default:
		p.add("%s: log.format %q is not one of text, json", ConfigFileName, d.Config.Log.Format)
	}
}

func (d *Dir) validateWeb(p *problems) {
	if l := d.Config.Web.Listen; l != "" {
		if err := checkLoopback(l); err != nil {
			p.add("%s: web.listen: %v", ConfigFileName, err)
		}
	}
	z := d.Config.Web.Ziti
	if z == nil {
		return
	}
	if z.Network == "" {
		p.add("%s: web.ziti.network is required", ConfigFileName)
	} else if _, ok := d.Network(z.Network); !ok {
		p.add("%s: web.ziti.network %q has no identity in %s/", ConfigFileName, z.Network, NetworksDirName)
	}
	if z.Service == "" {
		p.add("%s: web.ziti.service is required", ConfigFileName)
	}
	if len(z.Hosts) == 0 {
		p.add("%s: web.ziti.hosts is required: the name(s) people reach the service by, from its intercept config", ConfigFileName)
	}
	for _, h := range z.Hosts {
		if err := checkWebHost(h); err != nil {
			p.add("%s: web.ziti.hosts: %v", ConfigFileName, err)
		}
	}
	for _, id := range z.Allow {
		if id == "" {
			p.add("%s: web.ziti.allow has an empty identity id", ConfigFileName)
		}
	}
	switch z.PolicyAccess {
	case AccessNone, AccessRead, AccessEdit:
	default:
		p.add("%s: web.ziti.policy_access %q is not one of none, read, edit", ConfigFileName, z.PolicyAccess)
	}
}

// validateStitches checks each stitch and that names and enabled ingress
// services are unique. It returns the enabled ingress services, as
// network/service -> stitch name.
func (d *Dir) validateStitches(p *problems) map[string]string {
	names := map[string]bool{}
	ingress := map[string]string{}
	for i, s := range d.Stitches {
		where := fmt.Sprintf("%s: stitch %q", StitchesFileName, s.Name)
		if s.Name == "" {
			where = fmt.Sprintf("%s: stitch #%d", StitchesFileName, i+1)
			p.add("%s has no name", where)
		} else if !ValidName(s.Name) {
			p.add("%s: name is not valid (lower case letters, digits, '-' and '_')", where)
		} else if names[s.Name] {
			p.add("%s: name is used more than once", where)
		}
		names[s.Name] = true
		d.validateStitch(p, where, s)

		if s.Disabled {
			continue
		}
		key := endpointKey(s.From.Network, s.From.Service)
		if other, ok := ingress[key]; ok {
			p.add("%s: binds %s, which stitch %q already binds", where, key, other)
		} else {
			ingress[key] = s.Name
		}
	}
	return ingress
}

// validateStitch checks one stitch's own settings; where names it in
// messages.
func (d *Dir) validateStitch(p *problems, where string, s Stitch) {
	for _, ep := range []struct{ side, network, service string }{
		{"from", s.From.Network, s.From.Service},
		{"to", s.To.Network, s.To.Service},
	} {
		if ep.network == "" {
			p.add("%s: %s.network is required", where, ep.side)
		} else if _, ok := d.Network(ep.network); !ok {
			p.add("%s: %s.network %q has no identity in %s/", where, ep.side, ep.network, NetworksDirName)
		}
		if ep.service == "" {
			p.add("%s: %s.service is required", where, ep.side)
		}
	}
	if s.From.Network != "" && s.From.Network == s.To.Network && s.From.Service == s.To.Service {
		p.add("%s: dials the service it binds", where)
	}
	switch s.From.Precedence {
	case PrecedenceDefault, PrecedenceRequired, PrecedenceFailed:
	default:
		p.add("%s: from.precedence %q is not one of default, required, failed", where, s.From.Precedence)
	}
	switch s.AppData {
	case AppDataPassthrough, AppDataStrip, AppDataAnnotate:
	default:
		p.add("%s: app_data %q is not one of passthrough, strip, annotate", where, s.AppData)
	}
	if s.IdleTimeout < 0 {
		p.add("%s: idle_timeout cannot be negative", where)
	}
	if s.To.ConnectTimeout < 0 {
		p.add("%s: to.connect_timeout cannot be negative", where)
	}
}

// findCycles reports chains of enabled stitches that lead back to where
// they started, e.g. A:x -> B:y plus B:y -> A:x. Every connection on such a
// chain would loop until something times out. Each enabled ingress is bound
// by exactly one stitch (validate checks that first), so every node has at
// most one way out and following it is enough.
func (d *Dir) findCycles() []error {
	byIngress := map[string]Stitch{}
	for _, s := range d.Stitches {
		if !s.Disabled {
			byIngress[endpointKey(s.From.Network, s.From.Service)] = s
		}
	}

	var errs []error
	reported := map[string]bool{}
	for _, start := range d.Stitches {
		if start.Disabled || reported[start.Name] {
			continue
		}
		path := []string{start.Name}
		seen := map[string]bool{start.Name: true}
		cur := start
		for {
			next, ok := byIngress[endpointKey(cur.To.Network, cur.To.Service)]
			if !ok {
				break
			}
			if next.Name == start.Name {
				for _, n := range path {
					reported[n] = true
				}
				errs = append(errs, fmt.Errorf("%s: stitches form a loop: %s -> %s", StitchesFileName, strings.Join(path, " -> "), start.Name))
				break
			}
			if seen[next.Name] {
				break // a loop that does not include start; reported from one of its members
			}
			seen[next.Name] = true
			path = append(path, next.Name)
			cur = next
		}
	}
	return errs
}

// HostName is the host part of a web.ziti.hosts entry, which may carry a
// port.
func HostName(h string) string {
	if name, _, err := net.SplitHostPort(h); err == nil {
		return name
	}
	return h
}

// checkWebHost accepts a host name, optionally with a port.
func checkWebHost(h string) error {
	name, port, err := net.SplitHostPort(h)
	hasPort := err == nil
	if !hasPort {
		name = h
	}
	if name == "" || strings.ContainsAny(name, "/ :[]") {
		return fmt.Errorf("%q is not a host name, or host:port", h)
	}
	if hasPort && !validPort(port) {
		return fmt.Errorf("%q: the port must be a number from 1 to 65535", h)
	}
	return nil
}

// validPort reports whether s is a TCP port number.
func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}

// checkLoopback accepts host:port where the host is a loopback address.
// The UI is plain HTTP; serving it beyond this machine is for the Ziti
// listener, which is encrypted and authenticated end to end.
func checkLoopback(hostport string) error {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return err
	}
	if !validPort(port) {
		return fmt.Errorf("%q is not a valid port", port)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("%q is not a loopback address (127.0.0.1, ::1 or localhost); the UI is only served locally", host)
	}
	return nil
}

func endpointKey(network, service string) string {
	return network + ":" + service
}
