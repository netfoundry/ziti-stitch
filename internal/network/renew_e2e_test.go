//go:build e2e

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

// Runs against the local harness:
//
//	./deploy/local-e2e.sh up
//	ZITI_STITCH_E2E_DIR=$PWD/.local-e2e go test -tags e2e ./internal/network/

package network

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openziti/edge-api/rest_client_api_client/current_api_session"
	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/sdk-golang/ziti/edge"

	"github.com/netfoundry/ziti-stitch/internal/e2etest"
)

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRenewInPlaceKeepsServing renews the certificate of an identity that
// is hosting a service, with a connection open, and checks that nothing
// running notices: the open connection, the existing listener, a fresh
// authentication and a fresh bind all keep working, the identity file on
// disk holds the new credentials, and the old ones are dead.
func TestRenewInPlaceKeepsServing(t *testing.T) {
	dir := e2etest.Dir(t)
	harnessFile := filepath.Join(dir, "b", "renew-b.json")
	hostFile := filepath.Join(t.TempDir(), "partner.json")
	oldFile := filepath.Join(t.TempDir(), "old.json")
	copyFile(t, harnessFile, hostFile)
	copyFile(t, harnessFile, oldFile)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	info, err := ReadInfo(hostFile)
	if err != nil {
		t.Fatal(err)
	}
	n := New("partner", hostFile, info.CertCommonName, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	runDone := make(chan struct{})
	go func() { n.Run(ctx); close(runDone) }()
	defer func() { cancel(); <-runDone }()

	zHost, err := n.Context(ctx)
	if err != nil {
		t.Fatal(err)
	}
	listen := func() edge.Listener {
		l, err := Listen(ctx, zHost, "renew", &ziti.ListenOptions{
			ConnectTimeout: 10 * time.Second, MaxTerminators: 1,
		}, 10*time.Second)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		// Listen only waits for the router if the SDK's listener reports
		// its established count; an SDK upgrade must not quietly stop that.
		if _, ok := l.(establishedCounter); !ok {
			t.Errorf("the SDK's listener (%T) no longer reports how many binds are established", l)
		}
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
			}
		}()
		return l
	}
	l := listen()

	zDial, err := Connect(filepath.Join(dir, "b", "stitch-b.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer zDial.Close()
	dial := func() edge.Conn {
		c, err := zDial.DialWithOptions("renew", &ziti.DialOptions{ConnectTimeout: 10 * time.Second})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return c
	}

	live := dial()
	defer live.Close()
	echo(t, live, "before renewal")

	impl := zHost.(*ziti.ContextImpl)
	clt := impl.CtrlClt
	cur, err := clt.API.CurrentAPISession.GetCurrentAPISession(current_api_session.NewGetCurrentAPISessionParams(), clt)
	if err != nil {
		t.Fatal(err)
	}
	before, err := ReadInfo(hostFile)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := currentCert(clt)
	if err != nil {
		t.Fatal(err)
	}
	n.fileMu.Lock()
	err = n.renew(ctx, clt, *cur.Payload.Data.AuthenticatorID, leaf.Subject.CommonName)
	n.fileMu.Unlock()
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	// The harness's copy now holds a dead certificate; keep it usable for
	// the next run whatever happens below.
	t.Cleanup(func() { copyFile(t, hostFile, harnessFile) })

	echo(t, live, "open connection after renewal")
	c2 := dial()
	echo(t, c2, "new connection to the existing listener")
	_ = c2.Close()

	// As if the session had expired: the SDK's own full authentication,
	// which presents the client certificate, must work with the reloaded
	// one and leave the routers in step with the new session.
	clt.ApiSession.Store(nil)
	if err := zHost.Authenticate(); err != nil {
		t.Fatalf("authenticating with the reloaded certificate: %v", err)
	}
	_ = l.Close()
	l2 := listen()
	defer l2.Close()
	c3 := dial()
	echo(t, c3, "new bind after re-authenticating")
	_ = c3.Close()

	if err := probe(hostFile); err != nil {
		t.Errorf("renewed identity file does not authenticate: %v", err)
	}
	if after, err := ReadInfo(hostFile); err != nil || after.CertCommonName != before.CertCommonName {
		t.Errorf("renewed certificate's common name = %q (%v), want %q as before", after.CertCommonName, err, before.CertCommonName)
	}
	if err := probe(oldFile); err == nil {
		t.Error("old certificate still authenticates")
	}
	if _, err := os.Stat(n.pendingPath()); !os.IsNotExist(err) {
		t.Errorf("pending file left behind (err=%v)", err)
	}
}

func echo(t *testing.T, c edge.Conn, msg string) {
	t.Helper()
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatalf("%s: write: %v", msg, err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("%s: read: %v", msg, err)
	}
	if string(buf) != msg {
		t.Fatalf("%s: echoed %q", msg, buf)
	}
}
