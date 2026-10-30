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
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSystemdUnit(t *testing.T) {
	unit := systemdUnit("/var/lib/ziti-stitch")
	for _, want := range []string{
		"ReadWritePaths=/var/lib/ziti-stitch\n",
		"ExecStart={{Path | cmdEscape}}{{range Arguments}} {{. | cmd}}{{end}}\n",
		"ExecReload=/bin/kill -HUP $MAINPID\n",
		"{{if UserName}}User={{UserName}}\n{{end}}Restart=always\n",
		"CapabilityBoundingSet=\n",
		"ProtectSystem=strict\n",
		"TimeoutStopSec=45\n",
		"WantedBy=multi-user.target\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q", want)
		}
	}
	// The drain timeout must fit inside systemd's stop timeout.
	if drainTimeout >= 45*time.Second {
		t.Errorf("drainTimeout %v does not fit in TimeoutStopSec=45", drainTimeout)
	}
}

func TestCheckUnitConfigDir(t *testing.T) {
	for _, ok := range []string{"/var/lib/ziti-stitch", "/opt/ziti_stitch-2/cfg"} {
		if err := checkUnitConfigDir(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"relative/dir", "/var/lib/my stitch", "/x/{{evil}}", `/x/"q"`, "/x/a\\b", "/x/tab\there", "/x/%h", "/x/$HOME"} {
		if err := checkUnitConfigDir(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCheckUnitExecutable(t *testing.T) {
	for _, ok := range []string{"/usr/local/bin/ziti-stitch", "/opt/ziti_stitch-2/bin/ziti-stitch"} {
		if err := checkUnitExecutable(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"/opt/100%/ziti-stitch", "/opt/$x/ziti-stitch", `/opt/"q"/ziti-stitch`, "/opt/a\\b/ziti-stitch", "/opt/o'neil/ziti-stitch", "/opt/my tools/ziti-stitch", "/opt/tab\tx/ziti-stitch"} {
		if err := checkUnitExecutable(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestServiceConfigForThisPlatform(t *testing.T) {
	dir := "/var/lib/ziti-stitch"
	if runtime.GOOS == "windows" {
		dir = `C:\ProgramData\ziti-stitch`
	}
	cfg, err := serviceConfig(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != serviceName || !slices.Equal(cfg.Arguments[:3], []string{"run", "--config-dir", dir}) {
		t.Errorf("config = %+v", cfg)
	}
	switch runtime.GOOS {
	case "linux":
		if !strings.Contains(cfg.Option["SystemdScript"].(string), "ReadWritePaths="+dir) {
			t.Error("no hardened unit")
		}
		if slices.Contains(cfg.Arguments, "--log-file") {
			t.Error("Linux logs to journald, not a file")
		}
	case "darwin":
		if !slices.Contains(cfg.Arguments, filepath.Join(macLogDir, "ziti-stitch.log")) || cfg.Option["KeepAlive"] != true {
			t.Errorf("config = %+v", cfg)
		}
	case "windows":
		if !slices.Contains(cfg.Arguments, windowsLogFile(dir)) || cfg.Option["OnFailure"] != "restart" {
			t.Errorf("config = %+v", cfg)
		}
		if _, err := serviceConfig(dir, "someone"); err == nil {
			t.Error("--user accepted on Windows")
		}
	}
}

func TestProgramStartStop(t *testing.T) {
	stopped := make(chan struct{})
	p := &program{run: func(ctx context.Context) {
		<-ctx.Done()
		close(stopped)
	}}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
		t.Fatal("run returned before Stop")
	case <-time.After(20 * time.Millisecond):
	}
	if err := p.Stop(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Stop returned before run finished")
	}
}

func TestOpenLogFileCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "ziti-stitch.log")
	lf, err := openLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lf.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := lf.Close(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "hello\n" {
		t.Errorf("log file = %q, %v", b, err)
	}
}

func TestOpenLogFileRefusesOneItCannotWrite(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory this user cannot write to")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if _, err := openLogFile(filepath.Join(dir, "ziti-stitch.log")); err == nil || !strings.Contains(err.Error(), "cannot write the log file") {
		t.Errorf("err = %v, want cannot write the log file", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestFallbackWriterKeepsWhatTheLogFileCannotTake(t *testing.T) {
	var fallback bytes.Buffer
	w := &fallbackWriter{w: failingWriter{}, fallback: &fallback}
	for _, line := range []string{"one\n", "two\n"} {
		if n, err := w.Write([]byte(line)); err != nil || n != len(line) {
			t.Errorf("Write(%q) = %d, %v", line, n, err)
		}
	}
	if want := "cannot write the log file, so logging here: disk full\none\ntwo\n"; fallback.String() != want {
		t.Errorf("fallback got %q, want %q", fallback.String(), want)
	}
}
