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
	"io"
	"log/slog"

	"github.com/sirupsen/logrus"

	"github.com/netfoundry/ziti-stitch/internal/config"
	"github.com/netfoundry/ziti-stitch/internal/events"
)

// newLogger returns our logger for cfg, writing to w, and sets up the
// Ziti SDK's logrus output to match. setLevel changes the level of both
// later. The SDK is chatty at info (every router connection), so it logs
// warnings and up unless we are at debug. If rec is not nil it keeps
// every record at info and up, whatever the level, for the web UI.
// config.Load has already validated cfg.
func newLogger(cfg config.Log, w io.Writer, rec *events.Recorder) (log *slog.Logger, setLevel func(level string)) {
	lv := new(slog.LevelVar)
	setLevel = func(name string) {
		var level slog.Level
		_ = level.UnmarshalText([]byte(name))
		lv.Set(level)
		if level <= slog.LevelDebug {
			logrus.SetLevel(logrus.InfoLevel)
		} else {
			logrus.SetLevel(logrus.WarnLevel)
		}
	}
	setLevel(cfg.Level)
	opts := &slog.HandlerOptions{Level: lv}

	logrus.SetOutput(w)
	var h slog.Handler
	if cfg.Format == "json" {
		logrus.SetFormatter(newSDKFormatter(&logrus.JSONFormatter{}))
		h = slog.NewJSONHandler(w, opts)
	} else {
		logrus.SetFormatter(newSDKFormatter(&logrus.TextFormatter{FullTimestamp: true}))
		h = slog.NewTextHandler(w, opts)
	}
	if rec != nil {
		h = rec.Handler(h, slog.LevelInfo)
	}
	return slog.New(h), setLevel
}
