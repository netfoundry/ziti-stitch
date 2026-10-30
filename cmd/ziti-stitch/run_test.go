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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/events"
)

// freeAddrs returns n distinct loopback addresses with ports nothing
// listens on: all are held until all are chosen, or one could come twice.
func freeAddrs(t *testing.T, n int) []string {
	t.Helper()
	addrs := make([]string, n)
	for i := range addrs {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		addrs[i] = ln.Addr().String()
	}
	return addrs
}

// testLog writes ziti-stitch's log to the test's, where a failure shows it.
type testLog struct{ t *testing.T }

func (w testLog) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

// A config that is invalid at startup still gets health probes (alive,
// not ready), and once it is fixed the web UI starts and probes say ready.
func TestRunRecoversFromAnInvalidConfigAtStartup(t *testing.T) {
	dir := t.TempDir()
	addrs := freeAddrs(t, 2)
	healthAddr, webAddr := addrs[0], addrs[1]
	listeners := "health: {listen: '" + healthAddr + "'}\nweb: {listen: '" + webAddr + "'}\n"
	writeConfig := func(content string) {
		if err := os.WriteFile(filepath.Join(dir, config.ConfigFileName), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("log: {level: loud}\n" + listeners)
	if _, err := config.Load(dir); err == nil {
		t.Fatal("Load succeeded; the test needs an invalid config")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		fb := config.Fallback(dir)
		rec := events.NewRecorder(10)
		log, setLevel := newLogger(fb.Config.Log, testLog{t}, rec)
		run(ctx, func() {}, fb, false, log, setLevel, rec)
	}()
	defer func() { cancel(); <-done }()

	status := func(url string) int {
		resp, err := http.Get(url)
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	waitFor := func(what, url string, want int) {
		t.Helper()
		got := 0
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if got = status(url); got == want {
				return
			}
		}
		t.Fatalf("%s: GET %s = %d, want %d", what, url, got, want)
	}

	waitFor("invalid config, alive", "http://"+healthAddr+"/healthz", http.StatusOK)
	waitFor("invalid config, not ready", "http://"+healthAddr+"/readyz", http.StatusServiceUnavailable)
	if got := status("http://" + webAddr + "/"); got != 0 {
		t.Fatalf("web UI answered (%d) while the config is invalid", got)
	}

	writeConfig(listeners)
	waitFor("fixed config, ready", "http://"+healthAddr+"/readyz", http.StatusOK)
	waitFor("fixed config, web UI up", "http://"+webAddr+"/login", http.StatusOK)
}
