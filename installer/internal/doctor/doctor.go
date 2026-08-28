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

// Package doctor runs the preflight checks: real probes with plain-language
// results and copy-paste remedies, following the onboarding TUI's doctor
// pattern.
package doctor

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Status classifies a check result.
type Status int

const (
	Pass Status = iota
	Warn
	Fail
)

// Result is the outcome of one check.
type Result struct {
	Status Status
	// Detail is a short human explanation ("Google Cloud SDK 502.0.0").
	Detail string
	// Fix is a copy-paste remedy shown when the check does not pass.
	Fix string
}

// Check is one named probe. Fatal checks block the install when they fail;
// the rest only warn.
type Check struct {
	Key   string
	Name  string
	Fatal bool
	Run   func(ctx context.Context) Result
}

const probeTimeout = 30 * time.Second

func output(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// Checks returns the preflight suite for a given vendored snapshot root.
func Checks(snapshotRoot string) []Check {
	return []Check{
		{
			Key: "gcloud", Name: "Google Cloud SDK", Fatal: true,
			Run: func(ctx context.Context) Result {
				out, err := output(ctx, "gcloud", "--version")
				if err != nil {
					return Result{Fail, "gcloud is not installed or not on PATH",
						"https://cloud.google.com/sdk/docs/install"}
				}
				return Result{Pass, strings.SplitN(out, "\n", 2)[0], ""}
			},
		},
		{
			Key: "adc", Name: "Application Default Credentials", Fatal: true,
			Run: func(ctx context.Context) Result {
				if _, err := output(ctx, "gcloud", "auth", "application-default", "print-access-token"); err != nil {
					return Result{Fail, "no application-default credentials; setup-gcp and ko need them",
						"gcloud auth application-default login"}
				}
				return Result{Pass, "credentials found", ""}
			},
		},
		{
			Key: "go", Name: "Go toolchain", Fatal: true,
			Run: func(ctx context.Context) Result {
				out, err := output(ctx, "go", "version")
				if err != nil {
					return Result{Fail, "go is not installed; the installer builds Substrate's images from source",
						"https://go.dev/dl/"}
				}
				have := goVersionOf(out)
				want := requiredGoVersion(snapshotRoot)
				if want != "" && !goVersionAtLeast(have, want) {
					return Result{Fail,
						fmt.Sprintf("go %s found, but the vendored substrate needs go >= %s", have, want),
						"https://go.dev/dl/"}
				}
				return Result{Pass, out, ""}
			},
		},
		{
			Key: "kubectl", Name: "kubectl", Fatal: false,
			Run: func(ctx context.Context) Result {
				if _, err := exec.LookPath("kubectl"); err != nil {
					return Result{Warn, "kubectl not found; needed for the verification step and day-2 use",
						"gcloud components install kubectl"}
				}
				return Result{Pass, "kubectl on PATH", ""}
			},
		},
		{
			Key: "git", Name: "git", Fatal: false,
			Run: func(ctx context.Context) Result {
				if _, err := exec.LookPath("git"); err != nil {
					return Result{Warn, "git not found; needed for fetching external overlays like Filestore CSI",
						"https://git-scm.com/downloads"}
				}
				return Result{Pass, "git on PATH", ""}
			},
		},
		{
			Key: "network", Name: "Network reachability", Fatal: true,
			Run: func(ctx context.Context) Result {
				ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				start := time.Now()
				if _, err := (&net.Resolver{}).LookupHost(ctx, "oauth2.googleapis.com"); err != nil {
					return Result{Fail, "cannot resolve oauth2.googleapis.com; Google APIs are unreachable",
						"check your network / VPN and retry"}
				}
				return Result{Pass, fmt.Sprintf("Google APIs resolvable (%dms)", time.Since(start).Milliseconds()), ""}
			},
		},
		{
			Key: "snapshot", Name: "Vendored substrate tree", Fatal: true,
			Run: func(ctx context.Context) Result {
				if _, err := os.Stat(filepath.Join(snapshotRoot, "go.mod")); err != nil {
					return Result{Fail, "vendored substrate/ tree not found at " + snapshotRoot,
						"hack/vendor-substrate.sh <substrate-checkout> main"}
				}
				return Result{Pass, snapshotRoot, ""}
			},
		},
	}
}

var goVersionRe = regexp.MustCompile(`go(\d+\.\d+(?:\.\d+)?)`)

func goVersionOf(versionOutput string) string {
	m := goVersionRe.FindStringSubmatch(versionOutput)
	if m == nil {
		return ""
	}
	return m[1]
}

// requiredGoVersion reads the `go` directive from the snapshot's go.mod.
func requiredGoVersion(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "go "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// goVersionAtLeast compares dotted version strings numerically. A missing
// segment counts as zero; unparseable input compares as satisfied so a probe
// never fails on a format surprise.
func goVersionAtLeast(have, want string) bool {
	if have == "" {
		return true
	}
	hp := strings.Split(have, ".")
	wp := strings.Split(want, ".")
	for i := 0; i < len(hp) || i < len(wp); i++ {
		h, w := 0, 0
		if i < len(hp) {
			h, _ = strconv.Atoi(hp[i])
		}
		if i < len(wp) {
			w, _ = strconv.Atoi(wp[i])
		}
		if h != w {
			return h > w
		}
	}
	return true
}

// RunCLI executes all checks sequentially, printing plain-text results for
// the --doctor mode. It returns the number of fatal failures.
func RunCLI(ctx context.Context, checks []Check) int {
	fatal := 0
	for _, c := range checks {
		res := c.Run(ctx)
		glyph := "✓"
		switch res.Status {
		case Warn:
			glyph = "!"
		case Fail:
			glyph = "✗"
			if c.Fatal {
				fatal++
			}
		}
		fmt.Printf("%s %-32s %s\n", glyph, c.Name, res.Detail)
		if res.Fix != "" && res.Status != Pass {
			fmt.Printf("    fix: %s\n", res.Fix)
		}
	}
	return fatal
}
