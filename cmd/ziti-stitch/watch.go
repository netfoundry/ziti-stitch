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
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/netfoundry/ziti-stitch/internal/config"
)

// reloadDebounce lets a burst of writes (an atomic write is a create and a
// rename; renaming a network touches two files) settle into one reload.
const reloadDebounce = 500 * time.Millisecond

// watchConfig calls changed, once things have been quiet for
// reloadDebounce, whenever a file ziti-stitch reads from dir is written,
// created, removed or renamed, and on any change at all in source, the
// directory config.yaml and stitches.yaml come from when it is not dir
// (config.EnvConfigSource; "" for none). A Kubernetes ConfigMap changes by
// swapping a hidden ..data symlink rather than the files' names, so every
// change there counts. It watches until ctx is done.
//
// Our own writes to identity files (renewal, controller lists) trigger it
// too; reloading then finds nothing changed, which costs a config read.
func watchConfig(ctx context.Context, dir, source string, changed func(), log *slog.Logger) error {
	dir = filepath.Clean(dir)
	networks := filepath.Join(dir, config.NetworksDirName)
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return err
	}
	if source != "" {
		source = filepath.Clean(source)
		if err := w.Add(source); err != nil {
			_ = w.Close()
			return err
		}
	}
	// It may not exist yet; it is added when it appears.
	_ = w.Add(networks)

	go func() {
		defer w.Close()
		// A stopped timer that each relevant event (re)arms.
		timer := time.AfterFunc(time.Hour, changed)
		timer.Stop()
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if ev.Name == networks && ev.Has(fsnotify.Create) {
					if err := w.Add(networks); err != nil {
						log.Warn("cannot watch the networks directory; reload with SIGHUP after adding networks", "error", err)
					}
				}
				if relevantPath(dir, ev.Name) || (source != "" && filepath.Dir(ev.Name) == source) {
					timer.Reset(reloadDebounce)
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Warn("watching the config directory", "error", err)
			}
		}
	}()
	return nil
}

// relevantPath reports whether path, inside the config directory dir, is
// something ziti-stitch reads: config.yaml, stitches.yaml, the networks
// directory, or an identity file in it (not a temporary file).
func relevantPath(dir, path string) bool {
	base := filepath.Base(path)
	switch filepath.Dir(path) {
	case dir:
		return base == config.ConfigFileName || base == config.StitchesFileName || base == config.NetworksDirName
	case filepath.Join(dir, config.NetworksDirName):
		return !strings.HasPrefix(base, ".") && strings.HasSuffix(base, config.IdentityExt)
	default:
		return false
	}
}
