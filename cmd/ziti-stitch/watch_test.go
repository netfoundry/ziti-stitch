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
	"runtime"
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
	if err := watchConfig(ctx, dir, "", func() { calls.Add(1) }, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
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

	// On Windows an event from the burst has arrived after the debounce
	// (once in CI), bringing a second reload. That costs only a config
	// read, so it is allowed there.
	maxCalls := int32(1)
	if runtime.GOOS == "windows" {
		maxCalls = 2
	}
	if n := settle(&calls, 0); n < 1 || n > maxCalls {
		t.Fatalf("changed called %d times, want once for the whole burst", n)
	}
	base := calls.Load()

	// An irrelevant file does nothing.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * reloadDebounce)
	if n := calls.Load(); n != base {
		t.Errorf("an unrelated file triggered a reload (%d calls, want %d)", n, base)
	}

	// A later change to an identity file in the new directory is seen.
	if err := os.WriteFile(filepath.Join(dir, "networks", "acme.json"), []byte("{ }"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := settle(&calls, base); n <= base || n > base+maxCalls {
		t.Errorf("identity change: %d calls, want %d", n, base+1)
	}
}

// settle waits, at most a few seconds, for calls to rise above from and
// then stay put for twice the debounce, and returns its value.
func settle(calls *atomic.Int32, from int32) int32 {
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() <= from && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	n := calls.Load()
	for time.Now().Before(deadline) {
		time.Sleep(2 * reloadDebounce)
		m := calls.Load()
		if m == n {
			return n
		}
		n = m
	}
	return n
}

// A Kubernetes ConfigMap update writes a new timestamped directory and
// swaps the ..data symlink to it; config.yaml itself is a symlink through
// ..data whose name never changes.
func TestWatchConfigSeesConfigMapUpdates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ConfigMap volumes are a Linux thing")
	}
	dir, src := t.TempDir(), t.TempDir()
	version := func(name, content string) {
		if err := os.Mkdir(filepath.Join(src, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, name, "stitches.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	version("..2026_10_14_01", "stitches: []\n")
	if err := os.Symlink("..2026_10_14_01", filepath.Join(src, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/stitches.yaml", filepath.Join(src, "stitches.yaml")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	if err := watchConfig(ctx, dir, src, func() { calls.Add(1) }, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}

	version("..2026_10_14_02", "stitches: [] # changed\n")
	if err := os.Symlink("..2026_10_14_02", filepath.Join(src, "..data_tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(src, "..data_tmp"), filepath.Join(src, "..data")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Error("a ConfigMap-style update did not trigger a reload")
	}
}
