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
	"testing"
	"time"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/network"
)

// haService exists on both harness networks for this test alone.
const haService = "ha"

// instance is one ziti-stitch instance's stitch, with its own identities.
type instance struct {
	*Stitch
	stop func() // stops the stitch and waits until its listener is gone
}

func startInstance(ctx context.Context, cfg config.Stitch, a, b *network.Network, log *slog.Logger) *instance {
	ctx, cancel := context.WithCancel(ctx)
	st := New(cfg, a, b, log)
	done := make(chan struct{})
	go func() { st.Run(ctx); close(done) }()
	return &instance{Stitch: st, stop: func() { cancel(); <-done; st.Wait() }}
}

func haStitch(precedence, egress string) config.Stitch {
	return config.Stitch{
		Name:    "ha",
		From:    config.Ingress{Network: "acme", Service: haService, Precedence: precedence},
		To:      config.Egress{Network: "partner", Service: egress, ConnectTimeout: 5 * time.Second},
		AppData: config.AppDataPassthrough,
	}
}

// waitListening waits until every instance is serving.
func waitListening(t *testing.T, instances ...*instance) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for _, in := range instances {
		for in.Status().State != StateListening {
			if time.Now().After(deadline) {
				t.Fatalf("instance not listening: %+v", in.Status())
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// TestTwoInstances runs two instances of the same stitch, each with its own
// identities, as docs/high-availability.md describes, and checks that A's
// dials keep working when one instance stops, and when one instance
// configured as the primary cannot reach the egress service.
func TestTwoInstances(t *testing.T) {
	dir := e2eDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	run := func(name, file string) *network.Network {
		return startNetwork(ctx, t, name, filepath.Join(dir, file), log)
	}
	a1, b1 := run("acme", "a/stitch-a.json"), run("partner", "b/stitch-b.json")
	a2, b2 := run("acme", "a/stitch-a2.json"), run("partner", "b/stitch-b2.json")

	zHost, err := run("host", "b/host-b.json").Context(ctx)
	if err != nil {
		t.Fatal(err)
	}
	startEchoHost(t, zHost, haService)
	zClient, err := run("client", "a/client-a.json").Context(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// dial makes n round trips and returns how many failed.
	dial := func(n int) int {
		failed := 0
		for i := 0; i < n; i++ {
			if got, err := roundTrip(zClient, haService, "", "ping"); err != nil || got != "ping" {
				t.Logf("dial %d: %q, %v", i, got, err)
				failed++
			}
		}
		return failed
	}

	i1 := startInstance(ctx, haStitch(config.PrecedenceDefault, haService), a1, b1, log)
	i2 := startInstance(ctx, haStitch(config.PrecedenceDefault, haService), a2, b2, log)
	defer i2.stop()
	waitListening(t, i1, i2)

	t.Run("both instances serve", func(t *testing.T) {
		if failed := dial(20); failed > 0 {
			t.Errorf("%d of 20 dials failed", failed)
		}
		t.Logf("accepted: instance 1 %d, instance 2 %d", i1.Status().Accepted, i2.Status().Accepted)
	})

	t.Run("one instance stops, the other takes every dial", func(t *testing.T) {
		i1.stop()
		before := i2.Status().Accepted
		if failed := dial(10); failed > 0 {
			t.Errorf("%d of 10 dials failed after instance 1 stopped", failed)
		}
		if got := i2.Status().Accepted - before; got != 10 {
			t.Errorf("instance 2 accepted %d of 10", got)
		}
	})

	t.Run("a primary that cannot reach the egress side hands over", func(t *testing.T) {
		// Instance 1 comes back as the required primary, dialing a service
		// nobody hosts on B: the gate must drop its precedence on the first
		// failure so the standby gets the dials.
		i1 = startInstance(ctx, haStitch(config.PrecedenceRequired, "ha-nobody-hosts"), a1, b1, log)
		defer i1.stop()
		waitListening(t, i1)
		if failed := dial(10); failed > 1 {
			t.Errorf("%d of 10 dials failed; the gate should cost at most one", failed)
		}
		if st := i1.Status(); st.State != StateGated {
			t.Errorf("instance 1 is %s, want gated", st.State)
		}
	})
}
