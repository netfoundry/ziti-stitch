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
	var info Info
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
	}
	return info, nil
}

// TokenController returns the controller an enrollment token is for,
// without redeeming it. Tokens work once, so callers settle everything
// they can (the network's name, say) before calling Enroll.
func TokenController(token string) (string, error) {
	claims, _, err := enroll.ParseToken(token)
	if err != nil {
		return "", fmt.Errorf("reading enrollment token: %w", err)
	}
	return claims.Issuer, nil
}

// Enroll redeems a one-time enrollment token (the JWT an admin hands out)
// with a fresh EC key, and returns the identity file content.
func Enroll(token string) ([]byte, error) {
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
