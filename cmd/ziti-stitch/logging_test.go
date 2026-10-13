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
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

func TestNewLoggerJSONAtInfo(t *testing.T) {
	var buf bytes.Buffer
	log, _ := newLogger(config.Log{Level: "info", Format: "json"}, &buf)
	log.Debug("hidden")
	log.Info("shown", "k", "v")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1:\n%s", len(lines), buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec["msg"] != "shown" || rec["k"] != "v" {
		t.Errorf("record = %v", rec)
	}
	if logrus.GetLevel() != logrus.WarnLevel {
		t.Errorf("SDK log level = %v, want warning", logrus.GetLevel())
	}
}

func TestNewLoggerTextAtDebug(t *testing.T) {
	var buf bytes.Buffer
	log, _ := newLogger(config.Log{Level: "debug", Format: "text"}, &buf)
	log.Debug("shown")
	if !strings.Contains(buf.String(), "msg=shown") {
		t.Errorf("output = %q", buf.String())
	}
	if logrus.GetLevel() != logrus.InfoLevel {
		t.Errorf("SDK log level = %v, want info", logrus.GetLevel())
	}
}

func TestSetLevelChangesBothLoggers(t *testing.T) {
	var buf bytes.Buffer
	log, setLevel := newLogger(config.Log{Level: "warn", Format: "text"}, &buf)
	log.Info("hidden")
	setLevel("debug")
	log.Debug("now shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "now shown") {
		t.Errorf("output = %q", buf.String())
	}
	if logrus.GetLevel() != logrus.InfoLevel {
		t.Errorf("SDK log level = %v, want info", logrus.GetLevel())
	}
}
