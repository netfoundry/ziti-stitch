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

package network

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/sdk-golang/ziti/enroll"
)

// Info is what an identity file says about itself, read without touching
// the network.
type Info struct {
	// Controllers are the client API URLs the identity knows, ztAPIs first.
	Controllers []string
	// CertNotAfter is when the certificate expires; zero when the file
	// refers to its certificate by path rather than holding it inline.
	CertNotAfter time.Time
	// CertCommonName is the certificate's common name, which renewal
	// keeps: for an identity enrolled with a one-time token, the
	// identity's id. Empty when the certificate is referred to by path.
	CertCommonName string
	// SelfContained reports whether the file holds its key, certificate
	// and CA inline (pem:), rather than referring to other files or an
	// HSM. Only then is copying the file enough.
	SelfContained bool
}

// ReadInfo reads the identity file at path.
func ReadInfo(path string) (Info, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Info{}, err
	}
	return parseInfo(raw)
}

func parseInfo(raw []byte) (Info, error) {
	var cfg ziti.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Info{}, fmt.Errorf("not an identity file: %w", err)
	}
	if cfg.ID.Key == "" || cfg.ID.Cert == "" {
		return Info{}, errors.New("not an enrolled identity: no key or certificate")
	}
	inline := func(v string) bool { return strings.HasPrefix(v, pemPrefix) }
	info := Info{SelfContained: inline(cfg.ID.Key) && inline(cfg.ID.Cert) && (cfg.ID.CA == "" || inline(cfg.ID.CA))}
	info.Controllers = append(info.Controllers, cfg.ZtAPIs...)
	if cfg.ZtAPI != "" && !slices.Contains(info.Controllers, cfg.ZtAPI) {
		info.Controllers = append(info.Controllers, cfg.ZtAPI)
	}
	if len(info.Controllers) == 0 {
		return Info{}, errors.New("identity file names no controller")
	}
	if inline, ok := strings.CutPrefix(cfg.ID.Cert, pemPrefix); ok {
		block, _ := pem.Decode([]byte(inline))
		if block == nil {
			return Info{}, errors.New("identity certificate is not PEM")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return Info{}, fmt.Errorf("identity certificate: %w", err)
		}
		info.CertNotAfter = cert.NotAfter
		info.CertCommonName = cert.Subject.CommonName
	}
	return info, nil
}

// enrollTimeout bounds each call into the SDK's enrollment code, whose
// HTTP clients have no timeout of their own: a controller that accepts the
// connection and never answers would hang enroll, and a pod's start, for
// good. Even reading a token contacts the controller (for the certificate
// that signed it).
var enrollTimeout = 2 * time.Minute

// errNoAnswer is withEnrollTimeout giving up.
var errNoAnswer = errors.New("the controller did not answer")

// withEnrollTimeout returns what f returns, or an error once enrollTimeout
// has passed. f is left running then; callers exit soon after.
func withEnrollTimeout[T any](f func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := f()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-time.After(enrollTimeout):
		var zero T
		return zero, fmt.Errorf("%w within %v", errNoAnswer, enrollTimeout)
	}
}

// TokenController returns the controller an enrollment token is for,
// without redeeming it. Tokens work once, so callers settle everything
// they can (the network's name, say) before calling Enroll.
func TokenController(token string) (string, error) {
	claims, err := withEnrollTimeout(func() (*ziti.EnrollmentClaims, error) {
		claims, _, err := enroll.ParseToken(token)
		return claims, err
	})
	if err != nil {
		return "", fmt.Errorf("reading enrollment token: %w", err)
	}
	return claims.Issuer, nil
}

// Enroll redeems a one-time enrollment token (the JWT an admin hands out)
// with a fresh EC key, and returns the identity file content.
func Enroll(token string) ([]byte, error) {
	raw, err := withEnrollTimeout(func() ([]byte, error) { return enrollToken(token) })
	if errors.Is(err, errNoAnswer) {
		return nil, fmt.Errorf("enrolling: %w; the token may have been used, and if so the network admin must issue a new one", err)
	}
	return raw, err
}

func enrollToken(token string) ([]byte, error) {
	claims, jwtToken, err := enroll.ParseToken(token)
	if err != nil {
		return nil, fmt.Errorf("reading enrollment token: %w", err)
	}
	if claims.EnrollmentMethod != "ott" {
		return nil, fmt.Errorf("enrollment method %q is not supported yet; only one-time tokens (ott) are", claims.EnrollmentMethod)
	}
	cfg, err := enroll.Enroll(enroll.EnrollmentFlags{
		Token:     claims,
		JwtToken:  jwtToken,
		JwtString: token,
		KeyAlg:    "EC",
	})
	if err != nil {
		return nil, fmt.Errorf("enrolling: %w", err)
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// DescribeExpiry renders a certificate expiry for people, e.g.
// "2027-10-12 (in 365 days)".
func DescribeExpiry(notAfter, now time.Time) string {
	if notAfter.IsZero() {
		return "unknown"
	}
	date := notAfter.Format(time.DateOnly)
	days := int(math.Round(notAfter.Sub(now).Hours() / 24))
	switch {
	case !now.Before(notAfter):
		return date + " (EXPIRED)"
	case days == 0:
		return date + " (today)"
	default:
		return fmt.Sprintf("%s (in %d days)", date, days)
	}
}
