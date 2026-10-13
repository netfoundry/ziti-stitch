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
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

func TestParseEndpoint(t *testing.T) {
	for in, want := range map[string][2]string{
		"acme:billing-api":     {"acme", "billing-api"},
		"acme:ERP Access":      {"acme", "ERP Access"},
		"acme:svc:with:colons": {"acme", "svc:with:colons"}, // only the first colon separates
	} {
		n, s, err := parseEndpoint(in)
		if err != nil || n != want[0] || s != want[1] {
			t.Errorf("parseEndpoint(%q) = %q, %q, %v; want %q, %q", in, n, s, err, want[0], want[1])
		}
	}
	for _, in := range []string{"acme", ":svc", "acme:", ""} {
		if _, _, err := parseEndpoint(in); err == nil {
			t.Errorf("parseEndpoint(%q) accepted", in)
		}
	}
}

func TestChooseName(t *testing.T) {
	d := &config.Dir{Networks: []config.Network{{Name: "acme"}}}
	const partner = "https://ctrl.partner.example.com:1280"
	if got, err := chooseName(d, "", partner); err != nil || got != "partner" {
		t.Errorf("derived name = %q, %v; want partner", got, err)
	}
	if got, err := chooseName(d, "mine", partner); err != nil || got != "mine" {
		t.Errorf("given name = %q, %v; want mine", got, err)
	}
	for name, ctrl := range map[string]string{
		"acme":   partner,                 // taken
		"Not OK": partner,                 // invalid
		"":       "https://10.0.0.5:1280", // nothing to derive a name from
	} {
		if got, err := chooseName(d, name, ctrl); err == nil {
			t.Errorf("chooseName(%q, %s) = %q, want an error", name, ctrl, got)
		}
	}
}

func TestKindOf(t *testing.T) {
	entry := func(level logrus.Level, msg string) *logrus.Entry {
		return &logrus.Entry{Level: level, Message: msg}
	}
	if kindOf(entry(logrus.ErrorLevel, "failed after 0ms")) != kindOf(entry(logrus.ErrorLevel, "failed after 125ms")) {
		t.Error("lines that differ only in numbers should be one kind")
	}
	if kindOf(entry(logrus.ErrorLevel, "failed")) == kindOf(entry(logrus.WarnLevel, "failed")) {
		t.Error("the same message at different levels should be different kinds")
	}
	if kindOf(entry(logrus.ErrorLevel, "bind failed")) == kindOf(entry(logrus.ErrorLevel, "dial failed")) {
		t.Error("different messages should be different kinds")
	}
}
