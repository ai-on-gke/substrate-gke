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

// The substrate-gke installer: an interactive wizard that provisions GCP
// resources and installs the Agent Substrate control plane onto a GKE
// cluster, building from a pinned agent-substrate/substrate checkout that the
// install steps fetch on demand.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ai-on-gke/substrate-gke/installer/internal/doctor"
	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/gcp"
	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
	"github.com/ai-on-gke/substrate-gke/installer/internal/theme"
	"github.com/ai-on-gke/substrate-gke/installer/internal/ui"
)

func main() {
	var (
		doctorMode    = flag.Bool("doctor", false, "run the preflight checks and exit")
		dryRun        = flag.Bool("dry-run", false, "walk the full wizard without touching GCP (simulated commands)")
		substrateRoot = flag.String("substrate-root", "", "use an existing substrate checkout instead of fetching the pinned one")
	)
	flag.Parse()

	// Fail here even under --dry-run: a bad --substrate-root would otherwise
	// leave root empty, and an empty root reaches the exit summary as a
	// teardown command whose `cd` target is missing entirely.
	root, managed, err := snapshot.Root(*substrateRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if *doctorMode {
		fmt.Println("substrate-gke preflight doctor")
		fmt.Println()
		if fatal := doctor.RunCLI(context.Background(), doctor.Checks(root, managed)); fatal > 0 {
			fmt.Printf("\n%d fatal problem(s) found.\n", fatal)
			os.Exit(1)
		}
		fmt.Println("\nAll good — run the installer.")
		return
	}

	var runner execx.Runner = execx.Real{}
	if *dryRun {
		runner = execx.DryRun{}
	}

	deps := &ui.Deps{
		Setup:   state.NewSetup(),
		Runner:  runner,
		GCP:     &gcp.Client{DryRun: *dryRun},
		Builder: snapshot.NewBuilder(root, managed),
		Checks:  doctor.Checks(root, managed),
		DryRun:  *dryRun,
	}

	app := ui.NewApp(deps)
	if _, err := tea.NewProgram(app, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	// Only once the install actually worked, and never against a simulated
	// run, which fetched nothing and would otherwise delete real caches.
	if app.Completed && !*dryRun {
		if err := deps.Builder.Cleanup(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not tidy the substrate cache:", err)
		}
	}

	printSummary(app, deps.Setup, root)
}

// printSummary leaves a plain-text recap in the terminal after the alt
// screen closes, like the prototype's exit panel.
func printSummary(app *ui.App, st *state.Setup, root string) {
	if !app.Completed {
		fmt.Println("Setup exited early — nothing to summarize. Re-running the installer is safe.")
		return
	}
	fmt.Println(theme.Good.Render("Substrate installed."))
	fmt.Printf("  project:  %s\n  cluster:  %s (%s)\n  bucket:   gs://%s\n  registry: %s\n",
		st.ProjectID, st.ClusterName, st.Zone, st.BucketName, st.KoDockerRepo)
	if st.FilestoreCSIDeployed {
		fmt.Println("  filestore: CSI driver deployed (gcp-filestore-csi-driver)")
	}
	if st.AutoscaleEnabled {
		fmt.Printf("  autoscaling: %s, %d–%d nodes\n", st.NodePool, st.AutoscaleMin, st.AutoscaleMax)
	}
	if st.DemoDeployed {
		fmt.Println("  demo: counter deployed — see the next steps printed in the wizard.")
	}
	fmt.Printf("\nThe substrate checkout is at %s\n", root)
	fmt.Println("Tear down GCP resources with the upstream hack/teardown.sh, or delete")
	// This line is meant to be pasted into a shell, so the path is quoted —
	// a space in it would otherwise make the cd land somewhere else. The
	// line above is prose, and reads better unquoted.
	fmt.Printf("the control plane with: (cd %s && go run ./cmd/ate-setup delete ate-system)\n", snapshot.ShellQuote(root))
}
