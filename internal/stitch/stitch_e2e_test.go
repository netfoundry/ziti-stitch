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
//	make e2e-test

package stitch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/sdk-golang/ziti/edge"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/e2etest"
	"github.com/netfoundry/ziti-stitch/internal/network"
)

// e2eService exists on both harness networks and is used by nothing else,
// so no other process can be bound to it.
const e2eService = "e2e"

// startNetwork runs a network from a copy of a harness identity file, so
// that nothing the network writes back touches the harness.
func startNetwork(ctx context.Context, t *testing.T, name, harnessFile string, log *slog.Logger) *network.Network {
	t.Helper()
	b, err := os.ReadFile(harnessFile)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := network.ReadInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	n := network.New(name, path, info.CertCommonName, log)
	go n.Run(ctx)
	return n
}

// echoHost binds a service on network B and echoes every connection,
// recording what each dial carried.
type echoHost struct {
	l  edge.Listener
	mu sync.Mutex
	// seen holds the app data and dialer identity name of each connection.
	seen []struct{ appData, dialer string }
}

func startEchoHost(t *testing.T, z ziti.Context, service string) *echoHost {
	t.Helper()
	l, err := network.Listen(context.Background(), z, service, &ziti.ListenOptions{
		ConnectTimeout: 10 * time.Second, MaxTerminators: 1,
	}, 10*time.Second)
	if err != nil {
		t.Fatalf("echo host listen: %v", err)
	}
	h := &echoHost{l: l}
	go func() {
		for {
			c, err := l.AcceptEdge()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.seen = append(h.seen, struct{ appData, dialer string }{string(c.GetAppData()), c.GetDialerIdentityName()})
			h.mu.Unlock()
			go func() {
				_, _ = io.Copy(c, c)
				_ = c.CloseWrite()
				_ = c.Close()
			}()
		}
	}()
	return h
}

func (h *echoHost) last() (appData, dialer string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.seen) == 0 {
		return "", ""
	}
	s := h.seen[len(h.seen)-1]
	return s.appData, s.dialer
}

// roundTrip dials service on network A with appData, sends msg,
// half-closes, and returns what came back.
func roundTrip(z ziti.Context, service, appData, msg string) (string, error) {
	c, err := z.DialWithOptions(service, &ziti.DialOptions{ConnectTimeout: 10 * time.Second, AppData: []byte(appData)})
	if err != nil {
		return "", err
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return "", err
	}
	if _, err := c.Write([]byte(msg)); err != nil {
		return "", err
	}
	if err := c.CloseWrite(); err != nil {
		return "", err
	}
	b, err := io.ReadAll(c)
	return string(b), err
}

func TestStitchEndToEnd(t *testing.T) {
	dir := e2etest.Dir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// The stitch: binds e2e on A as stitch-a, dials e2e on B as stitch-b.
	acme := startNetwork(ctx, t, "acme", filepath.Join(dir, "a", "stitch-a.json"), log)
	partner := startNetwork(ctx, t, "partner", filepath.Join(dir, "b", "stitch-b.json"), log)
	st := New(config.Stitch{
		Name:    "e2e",
		From:    config.Ingress{Network: "acme", Service: e2eService, Precedence: config.PrecedenceDefault},
		To:      config.Egress{Network: "partner", Service: e2eService, ConnectTimeout: 5 * time.Second},
		AppData: config.AppDataPassthrough,
	}, acme, partner, log)
	runDone := make(chan struct{})
	go func() { st.Run(ctx); close(runDone) }()
	defer func() { cancel(); <-runDone; st.Wait() }()

	// The real service on B, and a client on A.
	hostNet := startNetwork(ctx, t, "host", filepath.Join(dir, "b", "host-b.json"), log)
	zHost, err := hostNet.Context(ctx)
	if err != nil {
		t.Fatal(err)
	}
	host := startEchoHost(t, zHost, e2eService)
	clientNet := startNetwork(ctx, t, "client", filepath.Join(dir, "a", "client-a.json"), log)
	zClient, err := clientNet.Context(ctx)
	if err != nil {
		t.Fatal(err)
	}

	const appData = `{"dst_protocol":"tcp","dst_ip":"100.64.0.5","dst_port":"443"}`
	var got string
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, err = roundTrip(zClient, e2eService, appData, "hello through the stitch")
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond) // the stitch may still be binding
	}
	if err != nil || got != "hello through the stitch" {
		t.Fatalf("round trip = %q, %v", got, err)
	}

	t.Run("passthrough reaches the host unchanged", func(t *testing.T) {
		if gotAppData, _ := host.last(); gotAppData != appData {
			t.Errorf("host saw app data %q, want %q", gotAppData, appData)
		}
	})

	t.Run("the host sees the stitch's identity", func(t *testing.T) {
		e2etest.RequireCallerIdentity(t)
		if _, dialer := host.last(); dialer != "stitch-b" {
			t.Errorf("host saw dialer %q, want stitch-b", dialer)
		}
	})

	// Runs late: from here on the stitch closes every connection it opens.
	t.Run("closing connections ends one the client holds open", func(t *testing.T) {
		c, err := zClient.DialWithOptions(e2eService, &ziti.DialOptions{ConnectTimeout: 10 * time.Second, AppData: []byte(appData)})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4)
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatalf("echo: %v", err)
		}
		if n := st.CloseConnections(); n != 1 {
			t.Errorf("CloseConnections = %d, want 1", n)
		}
		if _, err := c.Read(buf); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("the client's connection stayed open: read err = %v", err)
		}
	})

	t.Run("egress down gives the client only the generic error", func(t *testing.T) {
		_ = host.l.Close()
		time.Sleep(time.Second) // let the terminator go
		_, err := roundTrip(zClient, e2eService, appData, "anyone there?")
		if err == nil {
			t.Fatal("dial succeeded with no host on B")
		}
		if !strings.Contains(err.Error(), errUpstreamUnavailable.Error()) {
			t.Errorf("client error does not carry the generic message: %v", err)
		}
		if strings.Contains(err.Error(), "terminators") {
			t.Errorf("client error leaks egress network details: %v", err)
		}
	})
}
