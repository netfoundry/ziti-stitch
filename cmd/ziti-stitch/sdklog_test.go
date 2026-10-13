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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// sdkLogger returns a logrus logger using a fresh sdkFormatter, and the
// buffer it writes to.
func sdkLogger() (*logrus.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	l := logrus.New()
	l.SetOutput(&buf)
	l.SetFormatter(newSDKFormatter(&logrus.TextFormatter{DisableTimestamp: true}))
	return l, &buf
}

func TestSDKFormatterRedactsSecrets(t *testing.T) {
	l, buf := sdkLogger()
	l.WithFields(logrus.Fields{"token": "eyJsecret", "connId": 7}).Error("Failed to establish connection")
	out := buf.String()
	if strings.Contains(out, "eyJsecret") {
		t.Fatalf("token leaked: %s", out)
	}
	if !strings.Contains(out, `token="[redacted]"`) || !strings.Contains(out, "connId=7") {
		t.Errorf("output = %s", out)
	}
}

func TestRedactsEverySecretField(t *testing.T) {
	for _, f := range sdkSecretFields {
		e := logrus.NewEntry(logrus.New()).WithField(f, "eyJsecret")
		if r := redact(e); r.Data[f] != "[redacted]" {
			t.Errorf("%s not redacted: %v", f, r.Data)
		}
	}
}

func TestRedactDoesNotMutateEntry(t *testing.T) {
	e := logrus.NewEntry(logrus.New()).WithField("token", "eyJsecret")
	if r := redact(e); r.Data["token"] != "[redacted]" {
		t.Errorf("redacted copy = %v", r.Data)
	}
	if e.Data["token"] != "eyJsecret" {
		t.Error("redact changed the original entry")
	}
	plain := logrus.NewEntry(logrus.New()).WithField("connId", 1)
	if redact(plain) != plain {
		t.Error("an entry without secrets should come back as is")
	}
}

func TestSDKFormatterDropsOwnRefusals(t *testing.T) {
	l, buf := sdkLogger()
	l.WithError(errors.New("ziti-stitch: upstream service unavailable")).Error("Failed to establish connection")
	if buf.Len() != 0 {
		t.Errorf("own refusal was logged: %s", buf.String())
	}
	l.WithError(errors.New("router went away")).Error("Failed to establish connection")
	if !strings.Contains(buf.String(), "router went away") {
		t.Errorf("other SDK errors must still be logged: %q", buf.String())
	}
}

// at logs msg at time ts through f, bypassing logrus's own clock.
func at(f *sdkFormatter, ts time.Time, msg string) string {
	e := logrus.NewEntry(logrus.New())
	e.Time, e.Level, e.Message = ts, logrus.ErrorLevel, msg
	b, err := f.Format(e)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestSDKFormatterRateLimitsRepeats(t *testing.T) {
	f := newSDKFormatter(&logrus.TextFormatter{DisableTimestamp: true})
	t0 := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)

	var shown, notes int
	for i := range 1000 {
		// Same kind despite the changing number.
		out := at(f, t0.Add(time.Duration(i)*time.Millisecond), "creating listener failed after "+string(rune('0'+i%10))+"ms")
		shown += strings.Count(out, "creating listener failed")
		notes += strings.Count(out, "are suppressed for up to")
	}
	if shown != sdkLogBurst || notes != 1 {
		t.Fatalf("shown %d lines and %d notes in one window, want %d and 1", shown, notes, sdkLogBurst)
	}

	// The next window opens with a count of what was dropped.
	out := at(f, t0.Add(sdkLogWindow+time.Second), "creating listener failed after 0ms")
	if !strings.Contains(out, "suppressed 997 more SDK log lines") || !strings.Contains(out, "creating listener failed") {
		t.Errorf("first line of the next window = %q", out)
	}
}

func TestSDKFormatterKindsAreIndependent(t *testing.T) {
	f := newSDKFormatter(&logrus.TextFormatter{DisableTimestamp: true})
	t0 := time.Now()
	for range 10 {
		at(f, t0, "noisy")
	}
	if out := at(f, t0, "something else"); !strings.Contains(out, "something else") {
		t.Errorf("a different message was suppressed: %q", out)
	}
}

func TestSDKFormatterForgetsOldKinds(t *testing.T) {
	f := newSDKFormatter(&logrus.TextFormatter{DisableTimestamp: true})
	t0 := time.Now()
	for i := range sdkLogMaxKinds {
		at(f, t0, "kind "+strings.Repeat("x", i))
	}
	at(f, t0.Add(sdkLogWindow), "one more kind")
	if n := len(f.kinds); n != 1 {
		t.Errorf("tracking %d kinds after the sweep, want 1", n)
	}
}
