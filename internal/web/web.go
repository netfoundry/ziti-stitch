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
	"time"

	"github.com/netfoundry/ziti-stitch/internal/diagnose"
	"github.com/netfoundry/ziti-stitch/internal/events"
	"github.com/netfoundry/ziti-stitch/internal/runner"
)

//go:embed templates/*.html static/*
var content embed.FS

// Snapshotter reports what is running; *runner.Runner is the real one.
type Snapshotter interface {
	Snapshot() runner.Snapshot
}

// Options configure a Server.
type Options struct {
	ConfigDir string
	Listen    string // loopback host:port, as configured
	Version   string
	Runner    Snapshotter
	Events    *events.Recorder
	Log       *slog.Logger
}

// Server is the web UI.
type Server struct {
	dir      string
	listen   string
	version  string
	started  time.Time
	run      Snapshotter
	events   *events.Recorder
	log      *slog.Logger
	key      []byte
	pages    map[string]*template.Template
	allowed  map[string]bool // Host header values accepted
	probe    func(name, path string) diagnose.NetworkReport
	now      func() time.Time
	shutdown time.Duration
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
	allowed, err := allowedHosts(o.Listen)
	if err != nil {
		return nil, err
	}
	return &Server{
		dir:      o.ConfigDir,
		listen:   o.Listen,
		version:  o.Version,
		started:  time.Now(),
		run:      o.Runner,
		events:   o.Events,
		log:      o.Log.With("component", "web"),
		key:      key,
		pages:    pages,
		allowed:  allowed,
		probe:    diagnose.Probe,
		now:      time.Now,
		shutdown: 5 * time.Second,
	}, nil
}

// allowedHosts lists the Host headers a request may carry: the configured
// address and its loopback equivalents. Anything else is a request that
// reached us under another name, which for a localhost UI means DNS
// rebinding: a web page pointing its own domain at 127.0.0.1.
func allowedHosts(listen string) (map[string]bool, error) {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("web listen address: %w", err)
	}
	allowed := map[string]bool{listen: true}
	for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
		allowed[net.JoinHostPort(h, port)] = true
	}
	return allowed, nil
}

// Serve listens on the configured address until ctx is done.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), s.shutdown)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	s.log.Info("web UI listening", "address", "http://"+s.listen+"/")
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler returns the UI's routes behind its checks.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(content, "static")
	mux.Handle("GET /static/", cacheStatic(http.StripPrefix("/static/", http.FileServerFS(static))))
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.signedIn(s.handleLogout))

	mux.HandleFunc("GET /{$}", s.signedIn(s.handleOverview))
	mux.HandleFunc("GET /stitches", s.signedIn(s.handleStitches))
	mux.HandleFunc("POST /stitches", s.signedIn(s.handleStitchAdd))
	mux.HandleFunc("GET /stitches/{name}", s.signedIn(s.handleStitchEdit))
	mux.HandleFunc("POST /stitches/{name}", s.signedIn(s.handleStitchUpdate))
	mux.HandleFunc("POST /stitches/{name}/enable", s.signedIn(s.handleStitchSetEnabled(true)))
	mux.HandleFunc("POST /stitches/{name}/disable", s.signedIn(s.handleStitchSetEnabled(false)))
	mux.HandleFunc("POST /stitches/{name}/remove", s.signedIn(s.handleStitchRemove))
	mux.HandleFunc("GET /networks", s.signedIn(s.handleNetworks))
	mux.HandleFunc("POST /networks/{name}/test", s.signedIn(s.handleNetworkTest))
	mux.HandleFunc("GET /connections", s.signedIn(s.handleConnections))
	mux.HandleFunc("GET /events", s.signedIn(s.handleEvents))
	mux.HandleFunc("GET /docs/", s.signedIn(s.handleDocs))
	mux.HandleFunc("/", s.signedIn(func(w http.ResponseWriter, r *http.Request, v *view) {
		s.renderError(w, r, v, http.StatusNotFound, "There is no such page.")
	}))
	return s.guard(mux)
}

// guard refuses requests under a foreign Host name and sets the security
// headers every response carries.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowed[r.Host] {
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

// signedIn requires a session, and a matching CSRF token on POST. Pages
// are never cached: they show identity names and live state.
func (s *Server) signedIn(next pageHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		sess, ok := s.sessionFrom(r)
		if !ok {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			if !csrfOK(r, sess) {
				http.Error(w, "missing or wrong CSRF token; reload the page and try again", http.StatusForbidden)
				return
			}
		}
		next(w, r, &view{CSRF: sess.CSRF, SignedIn: true, Theme: themeFrom(r), Version: s.version})
	}
}

// view is what every page template gets.
type view struct {
	Title    string
	Nav      string // which sidenav entry is current
	Theme    string // "", "light" or "dark"
	CSRF     string
	SignedIn bool
	Version  string
	Flash    string
	Data     any
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
