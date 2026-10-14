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

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/netfoundry/ziti-stitch/internal/runner"
	"github.com/netfoundry/ziti-stitch/internal/stitch"
)

// healthHandler answers liveness and readiness probes (D16). The answers
// carry counts only: the endpoint has no authentication.
//
//   - /healthz: the runner answers, so the process is not wedged.
//   - /readyz: a valid configuration is running, every network is ready
//     and every enabled stitch is listening (or gated, which is still
//     serving as a last resort).
func healthHandler(snapshot func() runner.Snapshot, configOK func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		snapshot()
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if why := notReady(snapshot(), configOK()); why != "" {
			http.Error(w, why, http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// notReady says why s is not ready to serve, or "" if it is.
func notReady(s runner.Snapshot, configOK bool) string {
	if !configOK {
		return "the configuration is not valid"
	}
	down := 0
	for _, n := range s.Networks {
		if !n.Ready {
			down++
		}
	}
	if down > 0 {
		return fmt.Sprintf("%d of %d networks not ready", down, len(s.Networks))
	}
	idle := 0
	for _, st := range s.Stitches {
		if st.State != stitch.StateListening && st.State != stitch.StateGated {
			idle++
		}
	}
	if idle > 0 {
		return fmt.Sprintf("%d of %d stitches not listening", idle, len(s.Stitches))
	}
	return ""
}

// startHealth serves the probes on addr until ctx is done. One that cannot
// listen is logged and does not stop the stitches.
func startHealth(ctx context.Context, addr string, r *runner.Runner, configOK func() bool, log *slog.Logger) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("cannot listen for health probes", "address", addr, "error", err)
		return
	}
	srv := &http.Server{
		Handler:           healthHandler(r.Snapshot, configOK),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Error("health probes stopped", "error", err)
		}
	}()
	log.Info("serving health probes", "address", ln.Addr().String())
}
