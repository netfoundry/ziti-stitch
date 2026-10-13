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
	"strings"
	"testing"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

var testOrigin = origin{dialerIdentityID: "id1", dialerIdentityName: "client-a", circuitID: "circ1"}

const tunnelerAppData = `{"dst_protocol":"tcp","dst_ip":"100.64.0.5","dst_port":"443","src_ip":"100.64.0.1"}`

func TestPassthroughIsByteForByte(t *testing.T) {
	in := []byte(`{ "b":1,  "a" : "x" }`)
	out, err := egressAppData(config.AppDataPassthrough, in, "s", testOrigin)
	if err != nil || string(out) != string(in) {
		t.Fatalf("passthrough = %q, %v", out, err)
	}
}

func TestStripSendsNothing(t *testing.T) {
	out, err := egressAppData(config.AppDataStrip, []byte(tunnelerAppData), "s", testOrigin)
	if err != nil || out != nil {
		t.Fatalf("strip = %q, %v", out, err)
	}
}

func TestUnknownPolicy(t *testing.T) {
	if _, err := egressAppData("copy", nil, "s", testOrigin); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAnnotateKeepsOriginalValuesExactly(t *testing.T) {
	out, err := egressAppData(config.AppDataAnnotate, []byte(tunnelerAppData), "billing", testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	var orig map[string]json.RawMessage
	_ = json.Unmarshal([]byte(tunnelerAppData), &orig)
	for k, v := range orig {
		if string(doc[k]) != string(v) {
			t.Errorf("%s = %s, want %s", k, doc[k], v)
		}
	}
	var a annotation
	if err := json.Unmarshal(doc[annotationKey], &a); err != nil {
		t.Fatal(err)
	}
	want := annotation{Stitch: "billing", DialerIdentityID: "id1", DialerIdentityName: "client-a", CircuitID: "circ1", Hops: 1}
	if a != want {
		t.Errorf("annotation = %+v, want %+v", a, want)
	}
}

func TestAnnotateEmptyAndNull(t *testing.T) {
	for _, in := range []string{"", "null"} {
		out, err := egressAppData(config.AppDataAnnotate, []byte(in), "s", testOrigin)
		if err != nil {
			t.Fatalf("annotate(%q): %v", in, err)
		}
		if !strings.Contains(string(out), `"hops":1`) {
			t.Errorf("annotate(%q) = %s", in, out)
		}
	}
}

func TestAnnotateCountsHops(t *testing.T) {
	out, err := egressAppData(config.AppDataAnnotate, []byte(`{"ziti_stitch":{"stitch":"first","hops":3}}`), "second", testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"hops":4`) || !strings.Contains(string(out), `"stitch":"second"`) {
		t.Errorf("annotate = %s", out)
	}
}

func TestAnnotateRefusesLoops(t *testing.T) {
	in := []byte(`{"ziti_stitch":{"hops":8}}`)
	if _, err := egressAppData(config.AppDataAnnotate, in, "s", testOrigin); !errors.Is(err, errTooManyHops) {
		t.Fatalf("err = %v, want errTooManyHops", err)
	}
}

func TestAnnotateRejectsNonObjects(t *testing.T) {
	for _, in := range []string{"not json", `["a"]`, `"text"`} {
		if _, err := egressAppData(config.AppDataAnnotate, []byte(in), "s", testOrigin); err == nil || errors.Is(err, errTooManyHops) {
			t.Errorf("annotate(%s): err = %v, want a JSON error", in, err)
		}
	}
}
