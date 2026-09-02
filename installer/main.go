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
	// Mark the tree as ours before anything can fetch it, so a concurrent
	// installer finishing first cannot tidy it away mid-install. Not under
	// --dry-run: a simulated run fetches nothing worth guarding, and even
	// Lock's sweep of orphaned staging directories would be a real deletion.
	if !*dryRun {
		deps.Builder.Lock()
	}

	app := ui.NewApp(deps)
	if _, err := tea.NewProgram(app, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	// Only once the install actually worked, and never against a simulated
	// run, which fetched nothing and would otherwise delete real caches.
	cleaned := false
	if app.Completed && !*dryRun {
		if err := deps.Builder.Cleanup(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not tidy the substrate cache:", err)
		} else {
			cleaned = true
		}
	}

	printSummary(app, deps.Setup, deps.Builder, cleaned)
}

// printSummary leaves a plain-text recap in the terminal after the alt
// screen closes, like the prototype's exit panel. cleaned reports whether
// Cleanup actually removed the managed tree — under --dry-run it never runs,
// and it can fail, so the summary must not claim more than happened.
func printSummary(app *ui.App, st *state.Setup, b *snapshot.Builder, cleaned bool) {
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
	// The managed checkout is scratch space, so point teardown at a command
	// that stands on its own. A checkout the user supplied is still where
	// they left it, and the pasted `cd` is quoted — a space in the path would
	// otherwise land it somewhere else; the prose mentions read better
	// unquoted.
	teardown := snapshot.TeardownCommand(st, "")
	switch {
	case b.Managed && cleaned:
		fmt.Printf("\nThe substrate tree was fetched to build your images and has been removed;\n")
		fmt.Printf("re-running the installer fetches it again. Develop against your own clone.\n")
	case b.Managed:
		fmt.Printf("\nThe substrate tree, if fetched, is cached at %s;\n", b.Root)
		fmt.Printf("it is removed once a real install succeeds. Develop against your own clone.\n")
	default:
		fmt.Printf("\nYour substrate checkout at %s is untouched.\n", b.Root)
		teardown = snapshot.TeardownCommand(st, b.Root)
	}
	// Two teardown depths: delete only the control plane (keep the cluster),
	// or delete everything this install created and stop the charges. The
	// cleanup invocation carries the wizard's own answers so it can be run
	// from a fresh clone weeks later.
	fmt.Printf("\nDelete the Substrate control plane (keeping the cluster) with:\n  %s\n", teardown)
	fmt.Println("\nDelete everything this install created in GCP — the cluster, the")
	fmt.Printf("snapshot bucket, IAM bindings, and dashboards — with:\n  %s\n", cleanupCommand(st))
}

// cleanupCommand renders the tools/cleanup-gcp invocation for this install.
// Quoted for pasting, like the teardown command.
func cleanupCommand(st *state.Setup) string {
	return fmt.Sprintf("./tools/cleanup-gcp --project %s --cluster %s --location %s --bucket %s",
		snapshot.ShellQuote(st.ProjectID), snapshot.ShellQuote(st.ClusterName),
		snapshot.ShellQuote(st.Zone), snapshot.ShellQuote(st.BucketName))
}
