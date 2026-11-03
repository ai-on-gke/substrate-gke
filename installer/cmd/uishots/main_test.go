// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The walk is a hand-copied key sequence that only `make screenshots` ran,
// so a wizard change that stalled it went unnoticed until the README's
// images were wrong (#86). Running it under `go test` catches that with the
// change that causes it.
func TestWalkWritesEveryScreen(t *testing.T) {
	out := t.TempDir()
	if err := run(out); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"welcome", "clusters", "guard", "provision", "complete"} {
		if _, err := os.Stat(filepath.Join(out, name+".ans")); err != nil {
			t.Errorf("%s.ans: %v", name, err)
		}
	}
}
