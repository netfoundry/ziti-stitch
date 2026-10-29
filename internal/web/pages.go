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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/diagnose"
	"github.com/netfoundry/ziti-stitch/internal/events"
	"github.com/netfoundry/ziti-stitch/internal/network"
	"github.com/netfoundry/ziti-stitch/internal/runner"
	"github.com/netfoundry/ziti-stitch/internal/stitch"
)

// stitchRow is a configured stitch and, if it is running, its status.
type stitchRow struct {
	Cfg    config.Stitch
	Status *stitch.Status
}

// netRow is a configured network and, if it is in use, its status.
type netRow struct {
	Name         string
	Controller   string
	CertNotAfter time.Time
	Status       *network.Status
	UsedBy       []string
}

// state is the configuration on disk joined with what is running.
type state struct {
	Dir       *config.Dir
	ConfigErr string // the config on disk is invalid; the last valid one is running
	Snap      runner.Snapshot
	Stitches  []stitchRow
	Networks  []netRow
}

func (s *Server) state() (*state, error) {
	d, err := config.Read(s.dir)
	if err != nil {
		return nil, err
	}
	st := &state{Dir: d, Snap: s.run.Snapshot()}
	if err := d.Validate(); err != nil {
		st.ConfigErr = err.Error()
	}
	running := map[string]stitch.Status{}
	for _, ss := range st.Snap.Stitches {
		running[ss.Name] = ss
	}
	for _, c := range d.Stitches {
		row := stitchRow{Cfg: c}
		if ss, ok := running[c.Name]; ok {
			row.Status = &ss
		}
		st.Stitches = append(st.Stitches, row)
	}
	nets := map[string]network.Status{}
	for _, ns := range st.Snap.Networks {
		nets[ns.Name] = ns
	}
	for _, n := range d.Networks {
		row := netRow{Name: n.Name, UsedBy: d.StitchesUsing(n.Name)}
		if info, err := network.ReadInfo(n.IdentityFile); err == nil {
			row.Controller, row.CertNotAfter = info.Controllers[0], info.CertNotAfter
		}
		if ns, ok := nets[n.Name]; ok {
			row.Status = &ns
			if !ns.CertNotAfter.IsZero() {
				row.CertNotAfter = ns.CertNotAfter
			}
		}
		st.Networks = append(st.Networks, row)
	}
	return st, nil
}

// loginData is what the sign-in page shows.
type loginData struct {
	Err    string
	Caller *caller // the Ziti identity that has no access, over Ziti
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	v := &view{Title: "Sign in", Theme: themeFrom(r), Version: s.version}
	var data loginData
	if c, ok := callerFrom(r); ok {
		data.Caller = &c
	}
	token := r.URL.Query().Get("t")
	if token == "" {
		if s.authorize(w, r).access != accessNone {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		v.Data = data
		s.render(w, http.StatusOK, "login", "", v)
		return
	}
	if !s.redeemLoginToken(token) {
		data.Err = "That sign-in link is not valid: it may have been used already or expired."
		v.Data = data
		s.render(w, http.StatusUnauthorized, "login", "", v)
		return
	}
	s.startSession(w, viaLink, "")
	attrs := []any{"remote", r.RemoteAddr}
	if data.Caller != nil {
		attrs = append(attrs, "ziti_identity", data.Caller.String())
	}
	s.log.Info("signed in to the web UI with a login link", attrs...)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, _ *view) {
	endSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type overviewData struct {
	*state
	Started   time.Time
	Enabled   int
	Listening int
	Ready     int
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request, v *view) {
	st, err := s.state()
	if err != nil {
		s.renderError(w, r, v, http.StatusInternalServerError, err.Error())
		return
	}
	data := overviewData{state: st, Started: s.started}
	for _, row := range st.Stitches {
		if !row.Cfg.Disabled {
			data.Enabled++
		}
		if row.Status != nil && (row.Status.State == stitch.StateListening || row.Status.State == stitch.StateGated) {
			data.Listening++
		}
	}
	for _, n := range st.Snap.Networks {
		if n.Ready {
			data.Ready++
		}
	}
	v.Title, v.Nav, v.Data = "Overview", "overview", data
	s.render(w, http.StatusOK, "overview", "", v)
}

// flashes are the messages a redirect may ask for, by code. The URL never
// carries the text itself, so a crafted link cannot put words on the page.
var flashes = map[string]string{
	"added":    "Added stitch %s.",
	"saved":    "Saved stitch %s.",
	"enabled":  "Enabled stitch %s.",
	"disabled": "Disabled stitch %s.",
	"removed":  "Removed stitch %s; its open connections finish undisturbed.",
}

func flashFrom(r *http.Request) string {
	q := r.URL.Query()
	format, ok := flashes[q.Get("done")]
	if !ok || !config.ValidName(q.Get("name")) {
		return ""
	}
	return fmt.Sprintf(format, q.Get("name")) + " The running stitches pick the change up on their own."
}

func redirectDone(w http.ResponseWriter, r *http.Request, done, name string) {
	http.Redirect(w, r, "/stitches?"+url.Values{"done": {done}, "name": {name}}.Encode(), http.StatusSeeOther)
}

// stitchForm is the add and edit form.
type stitchForm struct {
	IsNew          bool
	Stitch         config.Stitch
	ConnectTimeout string // as typed, so a bad value comes back for fixing
	IdleTimeout    string
	Networks       []string
	Err            string
	CSRF           string // the form is rendered on its own, without the view
	CanEdit        bool
}

func newStitchForm(d *config.Dir, s config.Stitch, isNew bool) stitchForm {
	f := stitchForm{IsNew: isNew, Stitch: s, ConnectTimeout: s.To.ConnectTimeout.String()}
	if s.IdleTimeout > 0 {
		f.IdleTimeout = s.IdleTimeout.String()
	}
	for _, n := range d.Networks {
		f.Networks = append(f.Networks, n.Name)
	}
	return f
}

type stitchesData struct {
	*state
	Form stitchForm
}

func (s *Server) handleStitches(w http.ResponseWriter, r *http.Request, v *view) {
	st, err := s.state()
	if err != nil {
		s.renderError(w, r, v, http.StatusInternalServerError, err.Error())
		return
	}
	v.Flash = flashFrom(r)
	s.renderStitches(w, http.StatusOK, v, st, newStitchForm(st.Dir, config.Stitch{}.WithDefaults(), true))
}

func (s *Server) renderStitches(w http.ResponseWriter, status int, v *view, st *state, f stitchForm) {
	f.CSRF, f.CanEdit = v.CSRF, v.CanEdit
	v.Title, v.Nav, v.Data = "Stitches", "stitches", stitchesData{state: st, Form: f}
	s.render(w, status, "stitches", "", v)
}

// parseStitchForm reads the add/edit form. name is the stitch's name when
// editing; adding takes it from the form.
func parseStitchForm(r *http.Request, name string) (config.Stitch, stitchForm, error) {
	f := stitchForm{ConnectTimeout: r.PostFormValue("connect_timeout"), IdleTimeout: r.PostFormValue("idle_timeout")}
	if name == "" {
		name = strings.TrimSpace(r.PostFormValue("name"))
	}
	s := config.Stitch{
		Name: name,
		From: config.Ingress{
			Network:    r.PostFormValue("from_network"),
			Service:    strings.TrimSpace(r.PostFormValue("from_service")),
			Precedence: r.PostFormValue("precedence"),
		},
		To: config.Egress{
			Network: r.PostFormValue("to_network"),
			Service: strings.TrimSpace(r.PostFormValue("to_service")),
		},
		AppData:  config.AppDataPolicy(r.PostFormValue("app_data")),
		Disabled: r.PostFormValue("disabled") == "on",
	}
	var errs []error
	if c := strings.TrimSpace(r.PostFormValue("cost")); c != "" {
		n, err := strconv.ParseUint(c, 10, 16)
		if err != nil {
			errs = append(errs, fmt.Errorf("cost %q is not a number from 0 to 65535", c))
		}
		s.From.Cost = uint16(n)
	}
	if d := strings.TrimSpace(f.ConnectTimeout); d != "" {
		t, err := time.ParseDuration(d)
		if err != nil {
			errs = append(errs, fmt.Errorf("connect timeout %q is not a duration like 5s", d))
		}
		s.To.ConnectTimeout = t
	}
	if d := strings.TrimSpace(f.IdleTimeout); d != "" {
		t, err := time.ParseDuration(d)
		if err != nil {
			errs = append(errs, fmt.Errorf("idle timeout %q is not a duration like 1h", d))
		}
		s.IdleTimeout = t
	}
	s = s.WithDefaults()
	f.Stitch = s
	return s, f, errors.Join(errs...)
}

func (s *Server) handleStitchAdd(w http.ResponseWriter, r *http.Request, v *view) {
	cfg, form, err := parseStitchForm(r, "")
	if err == nil {
		err = config.Update(s.dir, func(d *config.Dir) error {
			if d.StitchIndex(cfg.Name) >= 0 {
				return fmt.Errorf("a stitch called %q already exists", cfg.Name)
			}
			d.Stitches = append(d.Stitches, cfg)
			return nil
		})
	}
	if err != nil {
		st, serr := s.state()
		if serr != nil {
			s.renderError(w, r, v, http.StatusInternalServerError, serr.Error())
			return
		}
		form.IsNew, form.Networks, form.Err = true, newStitchForm(st.Dir, cfg, true).Networks, err.Error()
		s.renderStitches(w, http.StatusUnprocessableEntity, v, st, form)
		return
	}
	s.log.Info("stitch added in the web UI", "stitch", cfg.Name, "by", v.Who)
	redirectDone(w, r, "added", cfg.Name)
}

func (s *Server) handleStitchEdit(w http.ResponseWriter, r *http.Request, v *view) {
	st, err := s.state()
	if err != nil {
		s.renderError(w, r, v, http.StatusInternalServerError, err.Error())
		return
	}
	name := r.PathValue("name")
	i := st.Dir.StitchIndex(name)
	if i < 0 {
		s.renderError(w, r, v, http.StatusNotFound, "There is no stitch called "+name+".")
		return
	}
	s.renderEdit(w, http.StatusOK, v, st, newStitchForm(st.Dir, st.Dir.Stitches[i], false))
}

type editData struct {
	Form   stitchForm
	Status *stitch.Status
}

func (s *Server) renderEdit(w http.ResponseWriter, status int, v *view, st *state, f stitchForm) {
	f.CSRF, f.CanEdit = v.CSRF, v.CanEdit
	data := editData{Form: f}
	for _, row := range st.Stitches {
		if row.Cfg.Name == f.Stitch.Name {
			data.Status = row.Status
		}
	}
	v.Title, v.Nav, v.Data = "Stitch "+f.Stitch.Name, "stitches", data
	s.render(w, status, "stitch_edit", "", v)
}

func (s *Server) handleStitchUpdate(w http.ResponseWriter, r *http.Request, v *view) {
	name := r.PathValue("name")
	cfg, form, err := parseStitchForm(r, name)
	if err == nil {
		err = config.Update(s.dir, func(d *config.Dir) error {
			i := d.StitchIndex(name)
			if i < 0 {
				return fmt.Errorf("there is no stitch called %q any more", name)
			}
			d.Stitches[i] = cfg
			return nil
		})
	}
	if err != nil {
		st, serr := s.state()
		if serr != nil {
			s.renderError(w, r, v, http.StatusInternalServerError, serr.Error())
			return
		}
		form.Networks, form.Err = newStitchForm(st.Dir, cfg, false).Networks, err.Error()
		s.renderEdit(w, http.StatusUnprocessableEntity, v, st, form)
		return
	}
	s.log.Info("stitch changed in the web UI", "stitch", name, "by", v.Who)
	redirectDone(w, r, "saved", name)
}

func (s *Server) handleStitchSetEnabled(enable bool) pageHandler {
	done := "disabled"
	if enable {
		done = "enabled"
	}
	return func(w http.ResponseWriter, r *http.Request, v *view) {
		name := r.PathValue("name")
		s.editAndRedirect(w, r, v, name, done, func(d *config.Dir, i int) {
			d.Stitches[i].Disabled = !enable
		})
	}
}

func (s *Server) handleStitchRemove(w http.ResponseWriter, r *http.Request, v *view) {
	s.editAndRedirect(w, r, v, r.PathValue("name"), "removed", func(d *config.Dir, i int) {
		d.Stitches = slices.Delete(d.Stitches, i, i+1)
	})
}

// editAndRedirect applies change to the stitch called name and sends the
// browser back to the list.
func (s *Server) editAndRedirect(w http.ResponseWriter, r *http.Request, v *view, name, done string, change func(d *config.Dir, i int)) {
	err := config.Update(s.dir, func(d *config.Dir) error {
		i := d.StitchIndex(name)
		if i < 0 {
			return fmt.Errorf("there is no stitch called %q", name)
		}
		change(d, i)
		return nil
	})
	if err != nil {
		s.renderError(w, r, v, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.log.Info("stitch "+done+" in the web UI", "stitch", name, "by", v.Who)
	redirectDone(w, r, done, name)
}

type networksData struct {
	*state
	Test *networkTest // the result of a test, when the page shows one
}

// networkTest is what testing one network found.
type networkTest struct {
	Name     string
	Report   diagnose.NetworkReport
	Services []serviceRow
	Findings []diagnose.Finding
}

type serviceRow struct {
	Name string
	diagnose.Access
}

func (s *Server) handleNetworks(w http.ResponseWriter, r *http.Request, v *view) {
	st, err := s.state()
	if err != nil {
		s.renderError(w, r, v, http.StatusInternalServerError, err.Error())
		return
	}
	v.Title, v.Nav, v.Data = "Networks", "networks", networksData{state: st}
	s.render(w, http.StatusOK, "networks", "", v)
}

func (s *Server) handleNetworkTest(w http.ResponseWriter, r *http.Request, v *view) {
	st, err := s.state()
	if err != nil {
		s.renderError(w, r, v, http.StatusInternalServerError, err.Error())
		return
	}
	name := r.PathValue("name")
	n, ok := st.Dir.Network(name)
	if !ok {
		s.renderError(w, r, v, http.StatusNotFound, "There is no network called "+name+".")
		return
	}
	report := s.testNetwork(n.Name, n.IdentityFile)
	test := &networkTest{Name: name, Report: report}
	for svc, a := range report.Services {
		test.Services = append(test.Services, serviceRow{Name: svc, Access: a})
	}
	slices.SortFunc(test.Services, func(a, b serviceRow) int { return strings.Compare(a.Name, b.Name) })
	if report.Err == nil {
		test.Findings = diagnose.Stitches(st.Dir.Stitches, map[string]diagnose.NetworkReport{name: report})
	}
	v.Title, v.Nav, v.Data = "Networks", "networks", networksData{state: st, Test: test}
	if isHTMX(r) {
		s.render(w, http.StatusOK, "networks", "network-test", v)
		return
	}
	s.render(w, http.StatusOK, "networks", "", v)
}

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request, v *view) {
	v.Title, v.Nav, v.Data = "Connections", "connections", s.run.Snapshot()
	if isHTMX(r) {
		s.render(w, http.StatusOK, "connections", "conn-table", v)
		return
	}
	s.render(w, http.StatusOK, "connections", "", v)
}

// maxEvents is how many events the events page shows at most.
const maxEvents = 300

type eventsData struct {
	Events   []events.Event
	Stitches []string
	Stitch   string // filter
	Level    string // filter: info, warn or error
	More     bool   // there were more than maxEvents matches
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, v *view) {
	q := r.URL.Query()
	data := eventsData{Stitch: q.Get("stitch"), Level: q.Get("level")}
	min := slog.LevelInfo
	switch data.Level {
	case "warn":
		min = slog.LevelWarn
	case "error":
		min = slog.LevelError
	default:
		data.Level = "info"
	}
	if d, err := config.Read(s.dir); err == nil {
		for _, st := range d.Stitches {
			data.Stitches = append(data.Stitches, st.Name)
		}
	}
	for _, e := range s.events.Events() {
		if e.Level < min || (data.Stitch != "" && e.Attr("stitch") != data.Stitch) {
			continue
		}
		if len(data.Events) == maxEvents {
			data.More = true
			break
		}
		data.Events = append(data.Events, e)
	}
	v.Title, v.Nav, v.Data = "Events", "events", data
	s.render(w, http.StatusOK, "events", "", v)
}
