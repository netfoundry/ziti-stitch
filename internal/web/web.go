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

// Package web serves the optional web UI: status, connections, events and
// docs, and editing stitches. It edits the same files the CLI does, through
// the same validation, and the running process applies them the same way.
//
// It listens on loopback, as a Ziti service, or both. On loopback, signing
// in takes a login link. Over Ziti, the router-verified identity of whoever
// dialed decides, by web.ziti.allow and policy_access; login links work
// there too.
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openziti/sdk-golang/ziti"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/diagnose"
	"github.com/netfoundry/ziti-stitch/internal/events"
	"github.com/netfoundry/ziti-stitch/internal/network"
	"github.com/netfoundry/ziti-stitch/internal/retry"
	"github.com/netfoundry/ziti-stitch/internal/runner"
)

//go:embed templates/*.html static/*
var content embed.FS

// Runner is what the UI needs from the running process; *runner.Runner is
// the real one.
type Runner interface {
	Snapshot() runner.Snapshot
	// Network returns a running network: the Ziti listener binds through it.
	Network(name string) (runner.Network, bool)
}

// Options configure a Server. The listeners (Web.Listen, Web.Ziti's
// network, service and hosts) are fixed for the server's life; who may use
// the Ziti one is read from the config directory on every request.
type Options struct {
	ConfigDir string
	Web       config.Web
	Version   string
	Runner    Runner
	Events    *events.Recorder
	Log       *slog.Logger
}

// Server is the web UI.
type Server struct {
	dir           string
	web           config.Web
	version       string
	started       time.Time
	run           Runner
	events        *events.Recorder
	log           *slog.Logger
	key           []byte
	pages         map[string]*template.Template
	loopbackHosts map[string]bool // Host headers the loopback listener answers to
	zitiHosts     map[string]bool // host names the Ziti listener answers to
	lastZiti      atomic.Pointer[config.WebZiti]
	probe         func(name, path string) diagnose.NetworkReport
	tests         networkTests
	managedIn     string // config.EnvConfigSource: the stitches cannot be edited here
	now           func() time.Time
}

// New returns a server for the config directory in o.
func New(o Options) (*Server, error) {
	key, err := loadOrCreateKey(o.ConfigDir)
	if err != nil {
		return nil, err
	}
	pages, err := parsePages()
	if err != nil {
		return nil, err
	}
	s := &Server{
		dir:           o.ConfigDir,
		web:           o.Web,
		version:       o.Version,
		started:       time.Now(),
		run:           o.Runner,
		events:        o.Events,
		log:           o.Log.With("component", "web"),
		key:           key,
		pages:         pages,
		loopbackHosts: map[string]bool{},
		zitiHosts:     map[string]bool{},
		probe:         diagnose.Probe,
		now:           time.Now,
		managedIn:     os.Getenv(config.EnvConfigSource),
	}
	if o.Web.Listen != "" {
		_, port, err := net.SplitHostPort(o.Web.Listen)
		if err != nil {
			return nil, fmt.Errorf("web listen address: %w", err)
		}
		// The configured address and its loopback equivalents. Anything
		// else reached us under another name, which for a localhost UI
		// means DNS rebinding: a web page pointing its domain at 127.0.0.1.
		s.loopbackHosts[o.Web.Listen] = true
		for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
			s.loopbackHosts[net.JoinHostPort(h, port)] = true
		}
	}
	if z := o.Web.Ziti; z != nil {
		for _, h := range z.Hosts {
			s.zitiHosts[strings.ToLower(config.HostName(h))] = true
		}
		s.lastZiti.Store(z)
	}
	return s, nil
}

// zitiBindTimeout is how long binding the admin service waits for its
// first terminator before counting as failed and being retried.
const zitiBindTimeout = 10 * time.Second

// Serve runs the configured listeners until ctx is done. Each fails on
// its own: a loopback address that is taken is logged, and the Ziti
// listener keeps retrying, like a stitch does.
func (s *Server) Serve(ctx context.Context) {
	// Over Ziti, anyone the service policy admits can connect, access or
	// not, so slow or idle connections must not be held open for long.
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second, // a network test logs in to a controller first
		IdleTimeout:       2 * time.Minute,
		ConnContext:       withCaller,
	}
	var wg sync.WaitGroup
	if s.web.Listen != "" {
		if ln, err := net.Listen("tcp", s.web.Listen); err != nil {
			s.log.Error("cannot listen for the web UI on loopback", "address", s.web.Listen, "error", err)
		} else {
			s.log.Info("web UI listening", "address", "http://"+s.web.Listen+"/")
			wg.Go(func() {
				if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
					s.log.Error("web UI stopped listening on loopback", "error", err)
				}
			})
		}
	}
	if s.web.Ziti != nil {
		wg.Go(func() { s.serveZiti(ctx, srv) })
	}

	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	wg.Wait()
}

// serveZiti binds the admin service and serves on it, rebinding with
// backoff whenever the bind fails or the listener closes. It fetches the
// network from the runner on every attempt, so a network replaced by a
// config change is picked up.
func (s *Server) serveZiti(ctx context.Context, srv *http.Server) {
	z := s.web.Ziti
	log := s.log.With("network", z.Network, "service", z.Service)
	b := retry.Backoff{Min: time.Second, Max: 30 * time.Second}
	for ctx.Err() == nil {
		ln, err := s.listenZiti(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			wait := b.Next()
			log.Warn("cannot bind the web UI's Ziti service; retrying", "error", err, "retry_in", wait)
			retry.Sleep(ctx, wait)
			continue
		}
		log.Info("web UI listening on Ziti", "hosts", z.Hosts)
		started := time.Now()
		err = srv.Serve(ln)
		if ctx.Err() != nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		if time.Since(started) >= time.Minute {
			b.Reset()
		}
		wait := b.Next()
		log.Warn("web UI's Ziti listener closed; rebinding", "error", err, "retry_in", wait)
		retry.Sleep(ctx, wait)
	}
}

func (s *Server) listenZiti(ctx context.Context) (net.Listener, error) {
	z := s.web.Ziti
	nw, ok := s.run.Network(z.Network)
	if !ok {
		return nil, fmt.Errorf("network %q is not running", z.Network)
	}
	zctx, err := nw.Context(ctx)
	if err != nil {
		return nil, err
	}
	opts := ziti.DefaultListenOptions()
	opts.ConnectTimeout = zitiBindTimeout
	return network.Listen(ctx, zctx, z.Service, opts, zitiBindTimeout)
}

// Handler returns the UI's routes behind its checks.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(content, "static")
	mux.Handle("GET /static/", cacheStatic(http.StripPrefix("/static/", http.FileServerFS(static))))
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.page(accessRead, s.handleLogout))

	mux.HandleFunc("GET /{$}", s.page(accessRead, s.handleOverview))
	mux.HandleFunc("GET /stitches", s.page(accessRead, s.handleStitches))
	mux.HandleFunc("POST /stitches", s.page(accessEdit, s.handleStitchAdd))
	mux.HandleFunc("GET /stitches/{name}", s.page(accessRead, s.handleStitchEdit))
	mux.HandleFunc("POST /stitches/{name}", s.page(accessEdit, s.handleStitchUpdate))
	mux.HandleFunc("POST /stitches/{name}/enable", s.page(accessEdit, s.handleStitchSetEnabled(true)))
	mux.HandleFunc("POST /stitches/{name}/disable", s.page(accessEdit, s.handleStitchSetEnabled(false)))
	mux.HandleFunc("POST /stitches/{name}/remove", s.page(accessEdit, s.handleStitchRemove))
	mux.HandleFunc("GET /networks", s.page(accessRead, s.handleNetworks))
	// Testing changes nothing, so reading is enough.
	mux.HandleFunc("POST /networks/{name}/test", s.page(accessRead, s.handleNetworkTest))
	mux.HandleFunc("GET /connections", s.page(accessRead, s.handleConnections))
	mux.HandleFunc("GET /events", s.page(accessRead, s.handleEvents))
	mux.HandleFunc("GET /docs/", s.page(accessRead, s.handleDocs))
	mux.HandleFunc("/", s.page(accessRead, func(w http.ResponseWriter, r *http.Request, v *view) {
		s.renderError(w, r, v, http.StatusNotFound, "There is no such page.")
	}))
	return s.guard(mux)
}

// guard refuses requests under a foreign Host name and sets the security
// headers every response carries.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r) {
			http.Error(w, "unexpected Host header", http.StatusMisdirectedRequest)
			return
		}
		h := w.Header()
		// No inline script or style anywhere, so the policy can refuse both:
		// an injected <script> would not run.
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// pageHandler is a handler for signed-in pages, given the request's view.
type pageHandler func(w http.ResponseWriter, r *http.Request, v *view)

// page requires at least min access, and the session's CSRF token on POST.
// Pages are never cached: they show identity names and live state.
func (s *Server) page(min access, next pageHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		a := s.authorize(w, r)
		if a.access == accessNone {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}
		v := &view{CSRF: a.sess.CSRF, SignedIn: true, CanEdit: a.access == accessEdit && s.managedIn == "",
			CanSignOut: a.link, Who: a.who, Theme: themeFrom(r), Version: s.version, ManagedIn: s.managedIn}
		if a.access < min {
			s.renderError(w, r, v, http.StatusForbidden, "Your access is read-only.")
			return
		}
		if min == accessEdit && s.managedIn != "" {
			s.renderError(w, r, v, http.StatusForbidden, "The stitches are managed in "+s.managedIn+", not here; change them there.")
			return
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			if !csrfOK(r, a.sess) {
				http.Error(w, "missing or wrong CSRF token; reload the page and try again", http.StatusForbidden)
				return
			}
		}
		next(w, r, v)
	}
}

// view is what every page template gets.
type view struct {
	Title      string
	Nav        string // which sidenav entry is current
	Theme      string // "", "light" or "dark"
	CSRF       string
	SignedIn   bool
	CanEdit    bool
	CanSignOut bool   // a login link's session; a Ziti identity's cannot end
	Who        string // who is signed in, for the topbar and the audit log
	ManagedIn  string // where the stitches are managed, when not here (config.EnvConfigSource)
	Version    string
	Flash      string
	Data       any
}

// themeCookie holds the theme override, set by console.js.
const themeCookie = "zs_theme"

func themeFrom(r *http.Request) string {
	if c, err := r.Cookie(themeCookie); err == nil && (c.Value == "light" || c.Value == "dark") {
		return c.Value
	}
	return ""
}

// render writes page with v; partial, if not "", names a template within it
// to render on its own (for htmx).
func (s *Server) render(w http.ResponseWriter, status int, page, partial string, v *view) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "unknown page "+page, http.StatusInternalServerError)
		return
	}
	name := "layout"
	if partial != "" {
		name = partial
	}
	// Rendered to a buffer first, so a template error is a clean 500
	// rather than half a page.
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, v); err != nil {
		s.log.Error("rendering page", "page", page, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) renderError(w http.ResponseWriter, _ *http.Request, v *view, status int, msg string) {
	v.Title, v.Data = http.StatusText(status), msg
	s.render(w, status, "error", "", v)
}

func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}
