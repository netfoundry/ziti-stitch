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

package stitch

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/sdk-golang/ziti/edge"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/e2etest"
	"github.com/netfoundry/ziti-stitch/internal/network"
)

// stallService exists on both harness networks for this test alone: its
// host on B accepts connections and never reads them.
const stallService = "stall"

// TestStalledHostDoesNotFreezeOtherStitches runs two stitches through the
// same two network contexts, as the runner does. When the host behind one
// stops reading, only that connection may back up: the other stitch keeps
// working, and closing the stalled connection ends it.
func TestStalledHostDoesNotFreezeOtherStitches(t *testing.T) {
	dir := e2etest.Dir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	run := func(name, file string) ziti.Context {
		z, err := startNetwork(ctx, t, name, filepath.Join(dir, file), log).Context(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return z
	}
	acme := startNetwork(ctx, t, "acme", filepath.Join(dir, "a", "stitch-a.json"), log)
	partner := startNetwork(ctx, t, "partner", filepath.Join(dir, "b", "stitch-b.json"), log)

	// Hosts and clients get contexts of their own, so the stitch's are the
	// only router connections the two flows share.
	startEchoHost(t, run("echo-host", "b/host-b.json"), e2eService)
	l, err := network.Listen(ctx, run("stall-host", "b/host-b.json"), stallService, &ziti.ListenOptions{
		ConnectTimeout: 10 * time.Second, MaxTerminators: 1,
	}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []edge.Conn
	defer func() {
		_ = l.Close()
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
	}()
	go func() {
		for {
			c, err := l.AcceptEdge()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // never read
			mu.Unlock()
		}
	}()

	stitchOf := func(service string, idle time.Duration) config.Stitch {
		return config.Stitch{
			Name:        service,
			From:        config.Ingress{Network: "acme", Service: service, Precedence: config.PrecedenceDefault},
			To:          config.Egress{Network: "partner", Service: service, ConnectTimeout: 5 * time.Second},
			AppData:     config.AppDataPassthrough,
			IdleTimeout: idle,
		}
	}
	stalled := startInstance(ctx, stitchOf(stallService, 0), acme, partner, log)
	other := startInstance(ctx, stitchOf(e2eService, 0), acme, partner, log)
	defer other.stop()
	waitListening(t, stalled, other)

	zClient := run("client", "a/client-a.json")
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, err := roundTrip(zClient, e2eService, "", "before")
		if err == nil && got == "before" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("round trip before the stall: %q, %v", got, err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// stall writes into the stalled host until nothing more is taken.
	zStallClient := run("stall-client", "a/client-a.json")
	stall := func() {
		t.Helper()
		c, err := zStallClient.DialWithOptions(stallService, &ziti.DialOptions{ConnectTimeout: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		buf := make([]byte, 32*1024)
		for written := 0; ; written += len(buf) {
			if written > 64<<20 {
				t.Fatal("64 MiB went into a host that reads nothing")
			}
			_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if _, err := c.Write(buf); err != nil {
				return // backed up: the deadline passed
			}
		}
	}

	stall()
	if n := stalled.Status().Active; n != 1 {
		t.Fatalf("stalled stitch has %d connections open, want 1", n)
	}
	start := time.Now()
	if got, err := roundTrip(zClient, e2eService, "", "during"); err != nil || got != "during" {
		t.Fatalf("round trip through the other stitch while one host stalls: %q, %v after %v", got, err, time.Since(start))
	}

	start = time.Now()
	if n := stalled.CloseConnections(); n != 1 {
		t.Errorf("closed %d connections, want 1", n)
	}
	if d := time.Since(start); d > 2*closeWait {
		t.Errorf("closing the stalled connection took %v", d)
	}
	waitFor(t, 5*time.Second, "the closed stalled connection to end", func() bool { return stalled.Status().Active == 0 })
	stalled.stop()

	// The idle timeout ends a stalled connection the same way.
	stalled = startInstance(ctx, stitchOf(stallService, 2*time.Second), acme, partner, log)
	defer stalled.stop()
	waitListening(t, stalled)
	stall()
	waitFor(t, 10*time.Second, "the idle timeout to end the stalled connection", func() bool { return stalled.Status().Active == 0 })
}

// waitFor fails t unless ok becomes true within d.
func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !ok(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
