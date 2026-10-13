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

package stitch

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

const (
	// annotationKey holds what annotate adds. Tunnelers ignore keys they do
	// not know, so it is safe to send to them.
	annotationKey = "ziti_stitch"
	// maxHops is how many annotating stitches a connection may pass through
	// before we assume it is looping.
	maxHops = 8
)

// errTooManyHops means the app data shows the connection has already
// passed through maxHops stitches.
var errTooManyHops = errors.New("connection has passed through too many stitches")

// annotation is the object annotate adds to the app data.
type annotation struct {
	Stitch             string `json:"stitch"`
	DialerIdentityID   string `json:"dialer_identity_id,omitempty"`
	DialerIdentityName string `json:"dialer_identity_name,omitempty"`
	CircuitID          string `json:"circuit_id,omitempty"`
	Hops               int    `json:"hops"`
}

// origin is what the ingress router told us about the dialer.
type origin struct {
	dialerIdentityID   string
	dialerIdentityName string
	circuitID          string
}

// egressAppData returns the app data to send on the egress dial.
//
// For annotate it returns errTooManyHops when the connection looks like it
// is looping, and another error when the ingress app data is not a JSON
// object (which leaves nowhere to put the annotation); the caller decides
// what to do about either.
func egressAppData(policy config.AppDataPolicy, in []byte, stitch string, o origin) ([]byte, error) {
	switch policy {
	case config.AppDataPassthrough:
		return in, nil
	case config.AppDataStrip:
		return nil, nil
	case config.AppDataAnnotate:
		return annotate(in, stitch, o)
	default:
		return nil, fmt.Errorf("unknown app data policy %q", policy)
	}
}

func annotate(in []byte, stitch string, o origin) ([]byte, error) {
	// RawMessage keeps every value exactly as the dialer sent it. Tunnelers
	// fail the whole object if a field they know has an unexpected type, so
	// nothing but our own key may change.
	doc := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &doc); err != nil {
			return nil, fmt.Errorf("app data is not a JSON object: %w", err)
		}
		if doc == nil { // the JSON literal null
			doc = map[string]json.RawMessage{}
		}
	}

	hops := 1
	if prev, ok := doc[annotationKey]; ok {
		var a annotation
		if json.Unmarshal(prev, &a) == nil {
			hops = a.Hops + 1
		}
	}
	if hops > maxHops {
		return nil, errTooManyHops
	}

	v, err := json.Marshal(annotation{
		Stitch:             stitch,
		DialerIdentityID:   o.dialerIdentityID,
		DialerIdentityName: o.dialerIdentityName,
		CircuitID:          o.circuitID,
		Hops:               hops,
	})
	if err != nil {
		return nil, err
	}
	doc[annotationKey] = v
	return json.Marshal(doc)
}
