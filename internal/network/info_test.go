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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"slices"
	"testing"
	"time"
)

func selfSignedPEM(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    notAfter.Add(-time.Hour),
		NotAfter:     notAfter,
	}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func identityJSON(t *testing.T, ztAPI string, ztAPIs []string, cert string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"ztAPI":  ztAPI,
		"ztAPIs": ztAPIs,
		"id":     map[string]string{"key": "pem:KEY", "cert": cert, "ca": "pem:CA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseInfo(t *testing.T) {
	expiry := time.Date(2027, 10, 13, 1, 2, 3, 0, time.UTC)
	raw := identityJSON(t, "https://a:1280/edge/client/v1",
		[]string{"https://b:1280/edge/client/v1", "https://a:1280/edge/client/v1"},
		pemPrefix+selfSignedPEM(t, expiry))
	info, err := parseInfo(raw)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"https://b:1280/edge/client/v1", "https://a:1280/edge/client/v1"}; !slices.Equal(info.Controllers, want) {
		t.Errorf("controllers = %v, want %v", info.Controllers, want)
	}
	if !info.CertNotAfter.Equal(expiry) {
		t.Errorf("expiry = %v, want %v", info.CertNotAfter, expiry)
	}
}

func TestParseInfoCertByPath(t *testing.T) {
	info, err := parseInfo(identityJSON(t, "https://a:1280/edge/client/v1", nil, "file:///etc/id.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.CertNotAfter.IsZero() || len(info.Controllers) != 1 {
		t.Errorf("info = %+v", info)
	}
}

func TestParseInfoRejects(t *testing.T) {
	tests := map[string][]byte{
		"not json":      []byte("nope"),
		"not enrolled":  []byte(`{"ztAPI":"https://a"}`),
		"no controller": identityJSON(t, "", nil, "pem:x"),
		"bad pem":       identityJSON(t, "https://a", nil, "pem:not a cert"),
	}
	for name, raw := range tests {
		if _, err := parseInfo(raw); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestDescribeExpiry(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	tests := map[time.Time]string{
		{}:                     "unknown",
		now.Add(-time.Hour):    "2026-10-12 (EXPIRED)",
		now.Add(3 * time.Hour): "2026-10-12 (today)",
		now.AddDate(0, 0, 30):  "2026-11-11 (in 30 days)",
		now.AddDate(1, 0, 0):   "2027-10-12 (in 365 days)",
	}
	for notAfter, want := range tests {
		if got := DescribeExpiry(notAfter, now); got != want {
			t.Errorf("DescribeExpiry(%v) = %q, want %q", notAfter, got, want)
		}
	}
}
