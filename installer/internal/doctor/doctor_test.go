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

package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGoVersionOf(t *testing.T) {
	if got := goVersionOf("go version go1.26.4 darwin/arm64"); got != "1.26.4" {
		t.Fatalf("goVersionOf = %q", got)
	}
	if got := goVersionOf("gibberish"); got != "" {
		t.Fatalf("goVersionOf(garbage) = %q", got)
	}
}

func TestGoVersionAtLeast(t *testing.T) {
	for _, tc := range []struct {
		have, want string
		ok         bool
	}{
		{"1.26.4", "1.26.3", true},
		{"1.26.3", "1.26.3", true},
		{"1.25.9", "1.26.3", false},
		{"1.26", "1.26.3", false},
		{"2.0", "1.26.3", true},
		{"", "1.26.3", true}, // unparseable output never blocks
	} {
		if got := goVersionAtLeast(tc.have, tc.want); got != tc.ok {
			t.Errorf("goVersionAtLeast(%q, %q) = %v, want %v", tc.have, tc.want, got, tc.ok)
		}
	}
}

func TestRequiredGoVersionReadsGoMod(t *testing.T) {
	dir := t.TempDir()
	mod := "module example.com/x\n\ngo 1.26.3\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := requiredGoVersion(dir); got != "1.26.3" {
		t.Fatalf("requiredGoVersion = %q", got)
	}
	if got := requiredGoVersion(t.TempDir()); got != "" {
		t.Fatalf("requiredGoVersion(missing) = %q", got)
	}
}

func TestChecksIncludeTheFatalSet(t *testing.T) {
	fatal := map[string]bool{}
	for _, c := range Checks(t.TempDir()) {
		if c.Fatal {
			fatal[c.Key] = true
		}
	}
	for _, key := range []string{"gcloud", "adc", "go", "network", "snapshot"} {
		if !fatal[key] {
			t.Errorf("check %q should be fatal", key)
		}
	}
}
