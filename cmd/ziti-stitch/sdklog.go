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
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sirupsen/logrus"

	"github.com/netfoundry/ziti-stitch/internal/stitch"
)

// sdkSecretFields are SDK log fields that carry credentials. The SDK logs
// the service session token, a bearer JWT, when an accept fails ("token"),
// and the bind session token at debug ("session").
var sdkSecretFields = []string{"token", "jwt", "apiSessionToken", "sessionToken", "session"}

const (
	// The SDK can log the same failure thousands of times a second (it
	// retries a bind whose permission was revoked with no backoff), so
	// each kind of line gets sdkLogBurst lines per sdkLogWindow.
	sdkLogWindow = 10 * time.Second
	sdkLogBurst  = 3
	// sdkLogMaxKinds bounds the rate limiter's memory; messages that embed
	// ids are each their own kind.
	sdkLogMaxKinds = 1000
)

// sdkFormatter wraps the SDK's logrus formatter. It redacts credentials,
// drops the SDK's ERROR echo of a dial we refused on purpose (our own log
// reports it at the right level, with context), and rate-limits repeats.
type sdkFormatter struct {
	logrus.Formatter

	mu    sync.Mutex
	kinds map[string]*logKind
}

// logKind tracks one kind of line in the current window.
type logKind struct {
	start time.Time
	count int
}

func newSDKFormatter(f logrus.Formatter) *sdkFormatter {
	return &sdkFormatter{Formatter: f, kinds: map[string]*logKind{}}
}

func (f *sdkFormatter) Format(e *logrus.Entry) ([]byte, error) {
	if err, ok := e.Data[logrus.ErrorKey]; ok && strings.HasPrefix(fmt.Sprint(err), stitch.RefusalPrefix) {
		return nil, nil
	}

	d := f.admit(e)
	if !d.emit {
		return nil, nil
	}
	var out bytes.Buffer
	if d.suppressed > 0 {
		if err := f.writeNote(&out, e, fmt.Sprintf("suppressed %d more SDK log lines like the next one in the last %v", d.suppressed, sdkLogWindow)); err != nil {
			return nil, err
		}
	}
	b, err := f.Formatter.Format(redact(e))
	if err != nil {
		return nil, err
	}
	out.Write(b)
	if d.lastOfBurst {
		if err := f.writeNote(&out, e, fmt.Sprintf("further SDK log lines like the previous one are suppressed for up to %v", sdkLogWindow)); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

// admission is the rate limiter's decision about one line.
type admission struct {
	emit        bool
	suppressed  int  // lines the previous window of this kind dropped
	lastOfBurst bool // no more of this kind until the window ends
}

// admit counts e against its kind's current window.
func (f *sdkFormatter) admit(e *logrus.Entry) admission {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := kindOf(e)
	k := f.kinds[key]
	if k == nil || e.Time.Sub(k.start) >= sdkLogWindow {
		var d admission
		if k != nil {
			d.suppressed = max(k.count-sdkLogBurst, 0)
		} else if len(f.kinds) >= sdkLogMaxKinds {
			f.sweep(e.Time)
		}
		f.kinds[key] = &logKind{start: e.Time, count: 1}
		d.emit, d.lastOfBurst = true, sdkLogBurst == 1
		return d
	}
	k.count++
	return admission{emit: k.count <= sdkLogBurst, lastOfBurst: k.count == sdkLogBurst}
}

// sweep forgets kinds whose window has ended. The caller holds mu.
func (f *sdkFormatter) sweep(now time.Time) {
	for key, k := range f.kinds {
		if now.Sub(k.start) >= sdkLogWindow {
			delete(f.kinds, key)
		}
	}
}

// writeNote formats a line of our own about e's kind, at e's level.
func (f *sdkFormatter) writeNote(out *bytes.Buffer, e *logrus.Entry, msg string) error {
	note := logrus.NewEntry(e.Logger)
	note.Time, note.Level, note.Message = e.Time, e.Level, msg
	b, err := f.Formatter.Format(note)
	if err != nil {
		return err
	}
	out.Write(b)
	return nil
}

// kindOf groups lines that differ only in numbers ("failed after 0ms" and
// "failed after 2ms" are the same kind).
func kindOf(e *logrus.Entry) string {
	return e.Level.String() + " " + strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return '#'
		}
		return r
	}, e.Message)
}

// redact returns e, or a copy of it with credential fields masked. The
// original entry is never changed: other hooks may still read it.
func redact(e *logrus.Entry) *logrus.Entry {
	var c *logrus.Entry
	for _, k := range sdkSecretFields {
		if _, ok := e.Data[k]; !ok {
			continue
		}
		if c == nil {
			cp := *e
			cp.Data = maps.Clone(e.Data)
			c = &cp
		}
		c.Data[k] = "[redacted]"
	}
	if c == nil {
		return e
	}
	return c
}
