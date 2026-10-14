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

package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// EnvConfigDir overrides the default config directory.
const EnvConfigDir = "ZITI_STITCH_CONFIG_DIR"

// EnvConfigSource names a read-only directory holding config.yaml and
// stitches.yaml in place of the config directory's own: a mounted
// Kubernetes ConfigMap, say. The config directory then keeps only the
// identities and runtime state, and the stitches cannot be edited through
// ziti-stitch.
const EnvConfigSource = "ZITI_STITCH_CONFIG_SOURCE"

const appDirName = "ziti-stitch"

// DefaultDir returns $ZITI_STITCH_CONFIG_DIR if set. Otherwise it returns
// the system location when running as root (Linux, macOS) or always on
// Windows, where the service runs as LocalSystem, and the user's config
// directory for everyone else.
func DefaultDir() (string, error) {
	if d := os.Getenv(EnvConfigDir); d != "" {
		return d, nil
	}
	switch runtime.GOOS {
	case "windows":
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, appDirName), nil
		}
		return filepath.Join(`C:\ProgramData`, appDirName), nil
	case "darwin":
		if os.Geteuid() == 0 {
			return filepath.Join("/Library/Application Support", appDirName), nil
		}
	default:
		if os.Geteuid() == 0 {
			return filepath.Join("/var/lib", appDirName), nil
		}
	}
	ucd, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(ucd, appDirName), nil
}
