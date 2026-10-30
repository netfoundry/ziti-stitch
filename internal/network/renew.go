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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/openziti/edge-api/rest_client_api_client/current_api_session"
	"github.com/openziti/edge-api/rest_model"
	"github.com/openziti/identity"
	apis "github.com/openziti/sdk-golang/edge-apis"
	"github.com/openziti/sdk-golang/ziti"

	"github.com/netfoundry/ziti-stitch/internal/atomicfile"
	"github.com/netfoundry/ziti-stitch/internal/retry"
)

const (
	renewCheckInterval = time.Hour
	renewRetryInterval = 5 * time.Minute
	// pendingRetryInterval is the retry while a renewal's outcome is
	// unknown: the controller may already reject the certificate the
	// running context holds.
	pendingRetryInterval = 30 * time.Second
	// renewRequestTimeout bounds each controller call made while renewing.
	renewRequestTimeout = 30 * time.Second
	// maxRenewWindow is how long before expiry a long-lived certificate is
	// renewed. Short-lived ones renew in the last third of their lifetime.
	maxRenewWindow = 30 * 24 * time.Hour
	// expiryWarning is when a certificate that still has not renewed starts
	// logging at ERROR on every attempt.
	expiryWarning = 7 * 24 * time.Hour
)

// renewalDue reports whether a certificate valid from notBefore to
// notAfter should be renewed at now.
func renewalDue(notBefore, notAfter, now time.Time) bool {
	window := min(notAfter.Sub(notBefore)/3, maxRenewWindow)
	return !now.Before(notAfter.Add(-window))
}

// keepRenewed checks the certificate now and every renewCheckInterval,
// renewing it when it nears expiry or the controller asks for it.
func (n *Network) keepRenewed(ctx context.Context, zctx ziti.Context) {
	impl, ok := zctx.(*ziti.ContextImpl)
	if !ok {
		n.log.Error("certificate renewal is unavailable; the identity will stop working when its certificate expires", "context_type", fmt.Sprintf("%T", zctx))
		// Returning early would end Run, which closes the context the
		// stitches are using.
		<-ctx.Done()
		return
	}
	clt := impl.CtrlClt
	for {
		wait := renewCheckInterval
		if err := n.renewIfNeeded(ctx, clt); err != nil {
			wait = renewRetryInterval
			if _, serr := os.Stat(n.pendingPath()); serr == nil {
				wait = pendingRetryInterval
			}
			level := slog.LevelWarn
			if leaf, lerr := currentCert(clt); lerr == nil && time.Until(leaf.NotAfter) < expiryWarning {
				level = slog.LevelError
			}
			n.problem(fmt.Errorf("certificate renewal failed: %w", err))
			n.log.Log(ctx, level, "certificate renewal failed; retrying", "error", err, "retry_in", wait)
		}
		if !retry.Sleep(ctx, wait) {
			return
		}
	}
}

func (n *Network) renewIfNeeded(ctx context.Context, clt *ziti.CtrlClient) error {
	n.fileMu.Lock()
	defer n.fileMu.Unlock()

	// A pending file, or a renewal, is the new identity's business once
	// the file holds another; this network is only draining.
	if err := n.checkIdentity(); err != nil {
		return err
	}

	if _, err := os.Stat(n.pendingPath()); err == nil {
		return n.resolvePending(clt)
	}

	leaf, err := currentCert(clt)
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, renewRequestTimeout)
	defer cancel()
	cur, err := clt.API.CurrentAPISession.GetCurrentAPISession(current_api_session.NewGetCurrentAPISessionParamsWithContext(reqCtx), clt)
	if err != nil {
		return fmt.Errorf("reading API session: %w", err)
	}
	sess := cur.Payload.Data
	requested := sess.IsCertExtendRequested || sess.IsCertKeyRollRequested
	due := renewalDue(leaf.NotBefore, leaf.NotAfter, time.Now())
	n.log.Debug("certificate check", "not_before", leaf.NotBefore, "not_after", leaf.NotAfter, "due", due,
		"extend_requested", sess.IsCertExtendRequested, "key_roll_requested", sess.IsCertKeyRollRequested)
	if !requested && !due {
		return nil
	}
	if sess.AuthenticatorID == nil || *sess.AuthenticatorID == "" {
		return errors.New("API session has no authenticator id")
	}
	n.log.Info("renewing certificate", "expires", leaf.NotAfter, "requested_by_controller", requested)
	return n.renew(ctx, clt, *sess.AuthenticatorID, leaf.Subject.CommonName)
}

// renew gets a certificate for a fresh key and switches to it. The caller
// holds fileMu.
//
// The order matters. The controller rejects the old certificate as soon as
// the new one is verified, so the new key and certificate go to disk (the
// pending file) before verifying, and replace the identity file only once
// verified. If verifying fails we cannot know whether the controller acted
// on it (the reply may be what was lost), so the pending file stays and
// resolvePending settles it on the next check. A crash at any point leaves
// a working file for connect to find.
//
// commonName is the current certificate's. The controller signs the subject
// we ask for, so it is kept: for an identity enrolled with a one-time token
// it is the identity's id, which tells a renewed identity from a different
// one in its file.
func (n *Network) renew(ctx context.Context, clt *ziti.CtrlClient, authenticatorID, commonName string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: commonName}}, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))

	extCtx, cancel := context.WithTimeout(ctx, renewRequestTimeout)
	defer cancel()
	ext, err := clt.API.CurrentAPISession.ExtendCurrentIdentityAuthenticator(&current_api_session.ExtendCurrentIdentityAuthenticatorParams{
		ID:      authenticatorID,
		Extend:  &rest_model.IdentityExtendEnrollmentRequest{ClientCertCsr: &csr},
		Context: extCtx,
	}, clt)
	if err != nil {
		return fmt.Errorf("requesting new certificate: %w", err)
	}
	certPEM := ext.Payload.Data.ClientCert

	raw, err := os.ReadFile(n.path)
	if err != nil {
		return err
	}
	updated, err := replaceCredentials(raw, keyPEM, certPEM, ext.Payload.Data.Ca)
	if err != nil {
		return err
	}
	// Until it is verified the old certificate still works, so giving up
	// here loses nothing.
	if err := n.whileIdentityExists(func() error {
		return atomicfile.WriteFile(n.pendingPath(), updated, IdentityFilePerm)
	}); err != nil {
		return fmt.Errorf("saving new certificate: %w", err)
	}

	// Not ctx: a shutdown arriving mid-verify should not cut short a
	// request the controller may already be acting on.
	verifyCtx, cancelVerify := context.WithTimeout(context.Background(), renewRequestTimeout)
	defer cancelVerify()
	if _, err := clt.API.CurrentAPISession.ExtendVerifyCurrentIdentityAuthenticator(&current_api_session.ExtendVerifyCurrentIdentityAuthenticatorParams{
		ID:      authenticatorID,
		Extend:  &rest_model.IdentityExtendValidateEnrollmentRequest{ClientCert: &certPEM},
		Context: verifyCtx,
	}, clt); err != nil {
		return fmt.Errorf("confirming new certificate (the next check works out whether it took): %w", err)
	}
	return n.promote(clt, updated)
}

// resolvePending settles a renewal whose verification outcome is unknown:
// whichever of the pending file and the identity file authenticates is the
// live one. The caller holds fileMu.
func (n *Network) resolvePending(clt *ziti.CtrlClient) error {
	pending := n.pendingPath()
	raw, err := os.ReadFile(pending)
	if err != nil {
		return err
	}
	pendingErr := probe(pending)
	if pendingErr == nil {
		n.log.Warn("an earlier certificate renewal did take effect; switching to it")
		return n.promote(clt, raw)
	}
	currentErr := probe(n.path)
	if currentErr == nil {
		if err := os.Remove(pending); err != nil {
			return fmt.Errorf("removing unconfirmed renewal: %w", err)
		}
		n.log.Warn("discarded a certificate renewal the controller never confirmed")
		return nil
	}
	return fmt.Errorf("cannot tell whether an earlier renewal took effect; neither certificate authenticates: pending: %v; current: %v", pendingErr, currentErr)
}

// promote makes the confirmed pending file the identity file and switches
// the running context to its credentials. The caller holds fileMu.
func (n *Network) promote(clt *ziti.CtrlClient, raw []byte) error {
	// From here the old certificate no longer works. A failed rename only
	// means the next start picks the pending file up instead.
	if err := n.whileIdentityExists(func() error { return atomicfile.Rename(n.pendingPath(), n.path) }); err != nil {
		n.log.Error("new certificate is confirmed but still in the pending file; it will be used on the next start", "error", err)
	}

	cfg, err := credentials(raw)
	if err != nil {
		return err
	}
	id, err := liveIdentity(clt)
	if err != nil {
		return fmt.Errorf("new certificate saved, but the running context cannot switch to it (restart to use it): %w", err)
	}
	id.Config.Key, id.Config.Cert = cfg.Key, cfg.Cert
	if cfg.CA != "" {
		id.Config.CA = cfg.CA
	}
	if err := id.Reload(); err != nil {
		return fmt.Errorf("new certificate saved, but reloading it failed (restart to use it): %w", err)
	}

	leaf, err := leafOf(id.Cert())
	if err != nil {
		return err
	}
	n.updateStatus(func(s *Status) { s.CertNotAfter = leaf.NotAfter })
	n.log.Info("certificate renewed", "expires", leaf.NotAfter)
	return nil
}

func (n *Network) pendingPath() string {
	return n.path + PendingSuffix
}

// probe reports whether the identity file at path authenticates.
func probe(path string) error {
	zctx, err := Connect(path)
	if err != nil {
		return err
	}
	zctx.Close()
	return nil
}

// liveIdentity returns the identity the SDK's TLS configs are built from,
// so that updating and reloading it switches every new connection over.
func liveIdentity(clt *ziti.CtrlClient) (*identity.ID, error) {
	p, ok := clt.Credentials.(apis.IdentityProvider)
	if !ok {
		return nil, fmt.Errorf("credentials of type %T carry no identity", clt.Credentials)
	}
	return unwrapIdentity(p.GetIdentity())
}

// unwrapIdentity finds the *identity.ID whose Config Reload reads. The SDK
// hands out a LazyIdentity, which loads an ID from a copy of its config on
// first use; updating the wrapper's config would change nothing.
func unwrapIdentity(i identity.Identity) (*identity.ID, error) {
	switch v := i.(type) {
	case *identity.ID:
		return v, nil
	case *identity.LazyIdentity:
		v.Cert() // forces the load, so v.Identity is set
		return unwrapIdentity(v.Identity)
	default:
		return nil, fmt.Errorf("identity of type %T cannot be reloaded", i)
	}
}

func currentCert(clt *ziti.CtrlClient) (*x509.Certificate, error) {
	p, ok := clt.Credentials.(apis.IdentityProvider)
	if !ok {
		return nil, fmt.Errorf("credentials of type %T carry no certificate", clt.Credentials)
	}
	return leafOf(p.GetIdentity().Cert())
}

func leafOf(c *tls.Certificate) (*x509.Certificate, error) {
	if c == nil || len(c.Certificate) == 0 {
		return nil, errors.New("identity has no certificate")
	}
	if c.Leaf != nil {
		return c.Leaf, nil
	}
	return x509.ParseCertificate(c.Certificate[0])
}
