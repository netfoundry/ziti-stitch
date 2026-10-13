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
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestRelevantPath(t *testing.T) {
	dir := filepath.FromSlash("/etc/ziti-stitch")
	p := func(rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }
	for _, rel := range []string{"config.yaml", "stitches.yaml", "networks", "networks/acme.json"} {
		if !relevantPath(dir, p(rel)) {
			t.Errorf("%s should trigger a reload", rel)
		}
	}
	for _, rel := range []string{".stitches.yaml.tmp-123", "notes.txt", "networks/.acme.json.tmp-1", "networks/acme.json.pending", "networks/sub/acme.json"} {
		if relevantPath(dir, p(rel)) {
			t.Errorf("%s should not trigger a reload", rel)
		}
	}
}

func TestWatchConfigDebouncesChanges(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	if err := watchConfig(ctx, dir, func() { calls.Add(1) }, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}

	// A burst of writes, a new networks directory and a file in it.
	for range 3 {
		if err := os.WriteFile(filepath.Join(dir, "stitches.yaml"), []byte("stitches: []\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "networks"), 0o700); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the watcher add the new directory
	if err := os.WriteFile(filepath.Join(dir, "networks", "acme.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(2 * reloadDebounce)
	if n := calls.Load(); n != 1 {
		t.Fatalf("changed called %d times, want once for the whole burst", n)
	}

	// An irrelevant file does nothing.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * reloadDebounce)
	if n := calls.Load(); n != 1 {
		t.Errorf("an unrelated file triggered a reload (%d calls)", n)
	}

	// A later change to an identity file in the new directory is seen.
	if err := os.WriteFile(filepath.Join(dir, "networks", "acme.json"), []byte("{ }"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("identity change: %d calls, want 2", n)
	}
}
