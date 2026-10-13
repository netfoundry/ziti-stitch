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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/netfoundry/ziti-stitch/internal/atomicfile"
)

// Signing in. On localhost there are no accounts: `ziti-stitch web login`
// writes a one-time token into the config directory and prints a link
// carrying it. Whoever can write the config directory can therefore sign
// in, which is exactly who can change the configuration anyway. The server
// redeems the token once and hands out a signed session cookie.

const (
	webDirName      = "web"         // in the config directory
	loginDirName    = "logins"      // pending login tokens, one file each
	keyFileName     = "session.key" // signs session cookies
	sessionCookie   = "zs_session"
	sessionLifetime = 12 * time.Hour
	loginLifetime   = 5 * time.Minute
	privateFilePerm = 0o600
	privateDirPerm  = 0o700
)

// NewLoginToken creates a one-time sign-in token for the UI serving the
// config directory dir, valid for five minutes, and returns it.
func NewLoginToken(dir string) (string, error) {
	token := randomToken()
	logins := filepath.Join(dir, webDirName, loginDirName)
	if err := os.MkdirAll(logins, privateDirPerm); err != nil {
		return "", err
	}
	exp := strconv.FormatInt(time.Now().Add(loginLifetime).Unix(), 10)
	if err := atomicfile.WriteFile(filepath.Join(logins, tokenFileName(token)), []byte(exp), privateFilePerm); err != nil {
		return "", err
	}
	return token, nil
}

// tokenFileName stores a token by its hash, so the directory listing does
// not hold usable tokens.
func tokenFileName(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// redeemLoginToken consumes token and reports whether it was valid. It
// also deletes expired tokens.
func (s *Server) redeemLoginToken(token string) bool {
	logins := filepath.Join(s.dir, webDirName, loginDirName)
	sweepExpiredTokens(logins, s.now())
	if token == "" {
		return false
	}
	path := filepath.Join(logins, tokenFileName(token))
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	// Removing first makes the token single-use even if two requests race
	// for it: only one remove succeeds.
	if err := os.Remove(path); err != nil {
		return false
	}
	exp, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return err == nil && s.now().Unix() < exp
}

func sweepExpiredTokens(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if exp, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err != nil || now.Unix() >= exp {
			_ = os.Remove(path)
		}
	}
}

// loadOrCreateKey returns the key that signs session cookies, creating it
// on first use. Deleting the file signs everybody out.
func loadOrCreateKey(dir string) ([]byte, error) {
	path := filepath.Join(dir, webDirName, keyFileName)
	key, err := os.ReadFile(path)
	if err == nil && len(key) == 32 {
		return key, nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, 32)
	rand.Read(key)
	if err := os.MkdirAll(filepath.Dir(path), privateDirPerm); err != nil {
		return nil, err
	}
	if err := atomicfile.WriteFile(path, key, privateFilePerm); err != nil {
		return nil, fmt.Errorf("saving the session key: %w", err)
	}
	return key, nil
}

// session is what the session cookie carries, signed.
type session struct {
	Exp  int64  `json:"e"`
	CSRF string `json:"c"` // must accompany every POST
}

func (s *Server) startSession(w http.ResponseWriter) {
	sess := session{Exp: s.now().Add(sessionLifetime).Unix(), CSRF: randomToken()}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.seal(sess), Path: "/",
		MaxAge: int(sessionLifetime / time.Second), HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func endSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// sessionFrom returns the request's valid session, if it has one.
func (s *Server) sessionFrom(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}
	payload, mac, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(mac), []byte(s.mac(payload))) {
		return session{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return session{}, false
	}
	var sess session
	if json.Unmarshal(raw, &sess) != nil || s.now().Unix() >= sess.Exp || sess.CSRF == "" {
		return session{}, false
	}
	return sess, true
}

func (s *Server) seal(sess session) string {
	raw, _ := json.Marshal(sess) // a struct of an int and a string cannot fail
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + s.mac(payload)
}

func (s *Server) mac(payload string) string {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// csrfOK reports whether a POST carries the session's CSRF token, as a form
// field or (from htmx) a header.
func csrfOK(r *http.Request, sess session) bool {
	got := r.Header.Get("X-CSRF-Token")
	if got == "" {
		got = r.PostFormValue("csrf")
	}
	return hmac.Equal([]byte(got), []byte(sess.CSRF))
}

// randomToken returns 256 random bits, URL-safe. (crypto/rand.Read never
// returns an error: it ends the program rather than return bad randomness.)
func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
