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

package web

import (
	"fmt"
	"html/template"
	"log/slog"
	"slices"
	"time"

	"github.com/netfoundry/ziti-stitch/internal/network"
	"github.com/netfoundry/ziti-stitch/internal/stitch"
)

// sharedTemplates are parsed with every page: the layout, and fragments
// more than one page uses.
var sharedTemplates = []string{"templates/layout.html", "templates/stitch_form.html"}

// pageNames are the templates in templates/, each rendered inside
// layout.html.
var pageNames = []string{
	"overview", "stitches", "stitch_edit", "networks", "connections",
	"events", "docs_index", "docs_page", "login", "error",
}

func parsePages() (map[string]*template.Template, error) {
	pages := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		t, err := template.New(name).Funcs(funcs).ParseFS(content, slices.Concat(sharedTemplates, []string{"templates/" + name + ".html"})...)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}
		pages[name] = t
	}
	return pages, nil
}

var funcs = template.FuncMap{
	"ago":        ago,
	"bytes":      humanBytes,
	"duration":   humanDuration,
	"stateClass": stateClass,
	"levelClass": levelClass,
	"expiry":     func(t time.Time) string { return network.DescribeExpiry(t, time.Now()) },
	"clock":      func(t time.Time) string { return t.Local().Format("Jan 2 15:04:05") },
	"since":      time.Since,
	"list":       func(xs ...string) []string { return xs },
}

// ago renders how long ago t was, coarsely: "just now", "4m ago".
func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	if d < 10*time.Second {
		return "just now"
	}
	return humanDuration(d) + " ago"
}

// humanDuration renders d at the precision people read: "45s", "12m",
// "3h 5m", "2d 4h".
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// stateClass maps a stitch state to a pill class.
func stateClass(s stitch.State) string {
	switch s {
	case stitch.StateListening:
		return "ok"
	case stitch.StateGated, stitch.StateBinding:
		return "warn"
	default:
		return "off"
	}
}

func levelClass(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "bad"
	case l >= slog.LevelWarn:
		return "warn"
	default:
		return "off"
	}
}
