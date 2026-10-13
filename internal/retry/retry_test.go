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

package retry

import (
	"context"
	"testing"
	"time"
)

func TestBackoffDoublesToMaxAndResets(t *testing.T) {
	b := Backoff{Min: time.Second, Max: 5 * time.Second}
	want := []time.Duration{1, 2, 4, 5, 5}
	for i, w := range want {
		if got := b.Next(); got != w*time.Second {
			t.Errorf("Next #%d = %v, want %v", i, got, w*time.Second)
		}
	}
	b.Reset()
	if got := b.Next(); got != time.Second {
		t.Errorf("Next after Reset = %v, want 1s", got)
	}
}

func TestSleepStopsWhenContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if Sleep(ctx, time.Hour) {
		t.Fatal("Sleep reported a full wait on a cancelled context")
	}
	if time.Since(start) > time.Second {
		t.Fatal("Sleep did not return promptly")
	}
}

func TestSleepCompletes(t *testing.T) {
	if !Sleep(context.Background(), time.Millisecond) {
		t.Fatal("Sleep reported an interrupted wait")
	}
}
