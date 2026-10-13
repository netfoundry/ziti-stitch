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
	"context"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/openziti/sdk-golang/ziti/edge"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

// access is what a request may do.
type access int

const (
	accessNone access = iota
	accessRead        // look, and test networks
	accessEdit        // change stitches too
)

// caller is the Ziti identity a request came from, as its router
// reported it: the dialer identity headers are set by the router from the
// authenticated session, unlike the caller id, which the dialer chooses.
type caller struct {
	ID, Name string
}

func (c caller) String() string {
	switch {
	case c.ID == "":
		return "unidentified Ziti identity" // routers before OpenZiti 2.0 do not say
	case c.Name == "":
		return c.ID
	default:
		return c.Name + " (" + c.ID + ")"
	}
}

type callerKey struct{}

// withCaller is the http.Server's ConnContext: connections accepted from a
// Ziti listener carry their dialer identity into every request on them.
func withCaller(ctx context.Context, c net.Conn) context.Context {
	if ec, ok := c.(edge.Conn); ok {
		return context.WithValue(ctx, callerKey{}, caller{ID: ec.GetDialerIdentityId(), Name: ec.GetDialerIdentityName()})
	}
	return ctx
}

// callerFrom returns the request's Ziti identity, and false for a request
// that came in on the loopback listener.
func callerFrom(r *http.Request) (caller, bool) {
	c, ok := r.Context().Value(callerKey{}).(caller)
	return c, ok
}

// authz is the outcome of authorizing a request.
type authz struct {
	access access
	sess   session // carries the CSRF token; empty when there is none
	who    string  // for the audit log and the topbar
	link   bool    // signed in with a login link, so signing out means something
}

// authorize works out what r may do. A login link's session may do
// anything, on either listener. Over Ziti, the dialer identity is checked
// against the current config on every request, so changing web.ziti.allow
// or policy_access takes effect at once; the session cookie only carries
// the CSRF token, and is issued here on a GET when there is none.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) authz {
	sess, hasSess := s.sessionFrom(r)
	if hasSess && sess.Via == viaLink {
		return authz{access: accessEdit, sess: sess, who: "login link", link: true}
	}
	c, overZiti := callerFrom(r)
	if !overZiti {
		return authz{}
	}
	a := authz{access: s.zitiAccess(c.ID), who: c.String()}
	if a.access == accessNone {
		return a
	}
	switch {
	case hasSess && sess.Via == viaZiti && sess.ID == c.ID:
		a.sess = sess
	case r.Method == http.MethodGet:
		a.sess = s.startSession(w, viaZiti, c.ID)
	}
	// Otherwise a POST without its identity's session: it has no CSRF
	// token to match, so it is refused.
	return a
}

// zitiAccess is what the dialer identity id may do under the current
// config. A config that does not parse right now (mid-edit, say) does not
// lock everybody out: the last settings that did parse still apply.
func (s *Server) zitiAccess(id string) access {
	z := s.currentZiti()
	switch {
	case z == nil:
		return accessNone
	case id != "" && slices.Contains(z.Allow, id):
		return accessEdit
	case z.PolicyAccess == config.AccessEdit:
		return accessEdit
	case z.PolicyAccess == config.AccessRead:
		return accessRead
	default:
		return accessNone
	}
}

func (s *Server) currentZiti() *config.WebZiti {
	if d, err := config.Read(s.dir); err == nil {
		s.lastZiti.Store(d.Config.Web.Ziti)
		return d.Config.Web.Ziti
	}
	return s.lastZiti.Load()
}

// hostAllowed checks the Host header against the listener the request
// came in on. The loopback listener answers to its own address; the Ziti
// one to the configured hosts (the port, if any, is the intercept's).
func (s *Server) hostAllowed(r *http.Request) bool {
	if _, overZiti := callerFrom(r); !overZiti {
		return s.loopbackHosts[r.Host]
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return s.zitiHosts[strings.ToLower(host)]
}
