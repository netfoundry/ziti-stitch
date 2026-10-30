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
	"fmt"
	"strings"
)

// systemdUnit returns the unit `service install` writes on Linux, for a
// config directory at configDir (absolute, and accepted by
// checkUnitConfigDir).
//
// It is a template for the service library's own small template engine,
// which fills in the executable, arguments and user; configDir is baked in
// here because ReadWritePaths needs it. Everything is locked down except
// what ziti-stitch uses: outbound network connections, and writing to its
// config directory (certificate renewal, controller lists).
func systemdUnit(configDir string) string {
	return fmt.Sprintf(`[Unit]
Description={{Description}}
Documentation=https://github.com/netfoundry/ziti-stitch
After=network-online.target
Wants=network-online.target
ConditionFileIsExecutable={{Path | cmdEscape}}

[Service]
Type=simple
ExecStart={{Path | cmdEscape}}{{range Arguments}} {{. | cmd}}{{end}}
ExecReload=/bin/kill -HUP $MAINPID
{{if UserName}}User={{UserName}}
{{end}}Restart=always
RestartSec=5
# Shutdown drains open connections for up to 30s.
TimeoutStopSec=45
LimitNOFILE=65536
UMask=0077

NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%s
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK

[Install]
WantedBy=multi-user.target
`, configDir)
}

// checkUnitConfigDir rejects config directories the unit cannot express
// safely: ReadWritePaths splits on whitespace, the template engine would
// read braces as its own, and systemd expands % (specifiers such as %h)
// and, in ExecStart, $ (variables), quoted or not.
func checkUnitConfigDir(dir string) error {
	if !strings.HasPrefix(dir, "/") {
		return fmt.Errorf("config directory %q must be an absolute path", dir)
	}
	if strings.ContainsAny(dir, " \t\n\"'{}\\%$") {
		return fmt.Errorf("config directory %q cannot contain whitespace, quotes, braces, backslashes, %% or $ when installed as a systemd service", dir)
	}
	return nil
}

// checkUnitExecutable rejects an executable path the unit cannot express,
// by the same rule as checkUnitConfigDir: the service library escapes only
// spaces (as \x20), which ConditionFileIsExecutable may not unescape, and
// systemd unquotes and expands % and $ in ExecStart.
func checkUnitExecutable(path string) error {
	if strings.ContainsAny(path, " \t\n\"'{}\\%$") {
		return fmt.Errorf("%q cannot contain whitespace, quotes, braces, backslashes, %% or $ when installed as a systemd service; move the binary (e.g. to /usr/local/bin)", path)
	}
	return nil
}
