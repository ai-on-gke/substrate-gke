// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SetupGCPPath is where setup-gcp lives in this repository, relative to its
// root. It was moved here from agent-substrate/substrate's tools/setup-gcp
// (agent-substrate/substrate#2304), so Bootstrap runs this copy rather than
// whatever the Substrate checkout carries: newer upstream trees no longer
// have one, and older ones would provision with an outdated copy.
const SetupGCPPath = "tools/setup-gcp"

// FindSetupGCP returns the absolute path of this repository's setup-gcp
// module. An explicit path wins and must hold the module. Otherwise each
// start directory and its ancestors are searched, which covers every way the
// installer is launched from a checkout of this repository: `go run .` and
// `make run` from installer/, and the binary `make build` writes to bin/.
func FindSetupGCP(explicit string, starts ...string) (string, error) {
	if explicit != "" {
		if !isSetupGCP(explicit) {
			return "", fmt.Errorf("%s is not the setup-gcp module (no go.mod or main.go)", explicit)
		}
		return filepath.Abs(explicit)
	}
	for _, start := range starts {
		if start == "" {
			continue
		}
		dir, err := filepath.Abs(start)
		if err != nil {
			continue
		}
		for {
			candidate := filepath.Join(dir, SetupGCPPath)
			if isSetupGCP(candidate) {
				return candidate, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("cannot find %s above %s; run the installer from a substrate-gke checkout or pass --setup-gcp",
		SetupGCPPath, strings.Join(starts, " or "))
}

func isSetupGCP(dir string) bool {
	for _, f := range []string{"go.mod", "main.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}
