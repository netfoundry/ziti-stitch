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

// Package diagnose checks configured stitches against what their
// identities can actually do, so that an operator without admin access
// can tell their network admin exactly which policy is missing.
package diagnose

import (
	"fmt"
	"slices"

	"github.com/openziti/edge-api/rest_model"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/network"
)

// Access is what an identity may do with a service.
type Access struct {
	Dial, Bind bool
}

// NetworkReport is what one network's identity can see.
type NetworkReport struct {
	Name string
	// Err is set when the identity could not authenticate; the other
	// fields are then empty.
	Err          error
	IdentityName string
	Services     map[string]Access // by service name
}

// Probe authenticates the identity at path and lists the services it can
// see.
func Probe(name, path string) NetworkReport {
	r := NetworkReport{Name: name}
	z, err := network.Connect(path)
	if err != nil {
		r.Err = err
		return r
	}
	defer z.Close()

	if id, err := z.GetCurrentIdentity(); err == nil && id.Name != nil {
		r.IdentityName = *id.Name
	}
	if err := z.RefreshServices(); err != nil {
		r.Err = fmt.Errorf("listing services: %w", err)
		return r
	}
	svcs, err := z.GetServices()
	if err != nil {
		r.Err = fmt.Errorf("listing services: %w", err)
		return r
	}
	r.Services = make(map[string]Access, len(svcs))
	for _, s := range svcs {
		if s.Name != nil {
			r.Services[*s.Name] = accessOf(s.Permissions)
		}
	}
	return r
}

func accessOf(perms []rest_model.DialBind) Access {
	return Access{
		Dial: slices.Contains(perms, rest_model.DialBindDial),
		Bind: slices.Contains(perms, rest_model.DialBindBind),
	}
}

// Severity grades a finding.
type Severity int

const (
	OK Severity = iota
	Problem
)

// Finding is one result of checking a stitch.
type Finding struct {
	Severity Severity
	Stitch   string
	Message  string
}

// Stitches checks every enabled stitch: its ingress identity needs Bind on
// the ingress service and its egress identity Dial on the egress service.
// Ends on networks with no report in reports are skipped, so checking one
// network means passing only its report.
func Stitches(stitches []config.Stitch, reports map[string]NetworkReport) []Finding {
	var findings []Finding
	for _, s := range stitches {
		if s.Disabled {
			continue
		}
		if r, ok := reports[s.From.Network]; ok {
			findings = append(findings, checkEndpoint(s.Name, r, s.From.Network, s.From.Service, "Bind"))
		}
		if r, ok := reports[s.To.Network]; ok {
			findings = append(findings, checkEndpoint(s.Name, r, s.To.Network, s.To.Service, "Dial"))
		}
	}
	return findings
}

func checkEndpoint(stitch string, r NetworkReport, network, service, need string) Finding {
	f := Finding{Stitch: stitch}
	who := "the identity"
	if r.IdentityName != "" {
		who = fmt.Sprintf("identity %q", r.IdentityName)
	}
	switch a, visible := r.Services[service]; {
	case r.Err != nil:
		f.Severity, f.Message = Problem, fmt.Sprintf("cannot check %s:%s: network %s did not authenticate: %v", network, service, network, r.Err)
	case !visible:
		// The client API lists only services the identity has some policy
		// for, so "missing" and "no access" look the same from here.
		f.Severity, f.Message = Problem, fmt.Sprintf("%s:%s is not visible to %s: the service does not exist, or no service policy gives %s %s on it", network, service, who, who, need)
	case need == "Bind" && !a.Bind:
		f.Severity, f.Message = Problem, fmt.Sprintf("%s can dial %s:%s but not bind it; it needs a Bind service policy", who, network, service)
	case need == "Dial" && !a.Dial:
		f.Severity, f.Message = Problem, fmt.Sprintf("%s can bind %s:%s but not dial it; it needs a Dial service policy", who, network, service)
	default:
		f.Message = fmt.Sprintf("%s has %s on %s:%s", who, need, network, service)
	}
	return f
}
