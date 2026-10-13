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

package e2etest

import "testing"

func TestHasCallerIdentity(t *testing.T) {
	for v, want := range map[string]bool{
		"v2.0.8": true, "2.1.0-pre2": true, "v10.0.0": true,
		"v1.6.21": false, "v0.33.1": false, "": false, "garbage": false,
	} {
		if got := hasCallerIdentity(v); got != want {
			t.Errorf("hasCallerIdentity(%q) = %v, want %v", v, got, want)
		}
	}
}
