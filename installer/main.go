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
		fetchTree     = flag.String("fetch-substrate", "", "fetch a substrate checkout into this directory and exit (the pinned commit, or --commit)")
		fetchCommit   = flag.String("commit", "", "with --fetch-substrate: the commit to fetch instead of the pinned one")
	)
	flag.Parse()

	if *fetchTree != "" {
		if err := snapshot.FetchTree(context.Background(), *fetchTree, *fetchCommit); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

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

	logger, err := execx.NewLogger("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not create log file:", err)
	} else {
		defer logger.Close()
	}

	var runner execx.Runner = &execx.Real{Log: logger}
	if *dryRun {
		runner = execx.DryRun{Log: logger}
	}

	logPath := ""
	if logger != nil {
		logPath = logger.Path()
	}

	deps := &ui.Deps{
		Setup:   state.NewSetup(),
		Runner:  runner,
		GCP:     &gcp.Client{DryRun: *dryRun},
		Builder: snapshot.NewBuilder(root, managed),
		Checks:  doctor.Checks(root, managed),
		DryRun:  *dryRun,
		LogPath: logPath,
	}
	if dir, err := snapshot.DefaultUpgradeDir(); err == nil {
		deps.UpgradeDir = dir
	} else {
		fmt.Fprintln(os.Stderr, "warning:", err)
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
	if d, ok := runner.(interface{ Drain() }); ok {
		d.Drain()
	}

	// Only once the install actually worked, and never against a simulated
	// run, which fetched nothing and would otherwise delete real caches.
	cleaned := false
	// An upgrade fetched nothing into the install cache, and its trees have
	// to stay for the runbook, so there is nothing to tidy.
	if app.Completed && !*dryRun && !deps.Setup.Upgrade {
		if err := deps.Builder.Cleanup(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not tidy the substrate cache:", err)
		} else {
			cleaned = true
		}
	}

	printSummary(app, deps, cleaned)
}

// printSummary leaves a recap in the terminal after the alt screen closes,
// like the prototype's exit panel, broken into headed sections so the facts,
// the teardown commands, and the demo walkthrough don't run together.
// cleaned reports whether Cleanup actually removed the managed tree — under
// --dry-run it never runs, and it can fail, so the summary must not claim
// more than happened.
func printSummary(app *ui.App, deps *ui.Deps, cleaned bool) {
	st, b := deps.Setup, deps.Builder
	if !app.Completed {
		fmt.Println("Setup exited early — nothing to summarize. Re-running the installer is safe.")
		if deps.LogPath != "" {
			fmt.Printf("\nDetailed command log written to:\n  %s\n", theme.Accent.Render(deps.LogPath))
		}
		return
	}
	if st.Upgrade {
		installedDir, nextDir := b.UpgradeTrees(deps.UpgradeDir, st)
		fmt.Println(theme.Good.Render("Upgrade prepared."))
		fmt.Println(b.UpgradeSummary(st, installedDir, nextDir))
		if deps.LogPath != "" {
			fmt.Printf("\n  log        %s\n", deps.LogPath)
		}
		return
	}
	section := func(title string) { fmt.Println("\n" + theme.Title.Render(title)) }
	note := func(lines ...string) {
		for _, l := range lines {
			fmt.Println(theme.Subtle.Render("  " + l))
		}
	}
	command := func(cmd string) { fmt.Println("  " + theme.CommandLine.Render(cmd)) }

	fmt.Println(theme.Good.Render(theme.GlyphDone + " Substrate installed"))

	section("Resources")
	fmt.Printf("  project    %s\n  cluster    %s (%s)\n  bucket     gs://%s\n  images     %s\n",
		st.ProjectID, st.ClusterName, st.Zone, st.BucketName, st.ImageSummary())
	if deps.LogPath != "" {
		fmt.Printf("  log        %s\n", deps.LogPath)
	}
	if st.FilestoreCSIDeployed {
		fmt.Println("  filestore  CSI driver deployed (gcp-filestore-csi-driver)")
	}
	if st.AutoscaleEnabled {
		fmt.Printf("  autoscale  %s, %d–%d nodes\n", st.NodePool, st.AutoscaleMin, st.AutoscaleMax)
	}
	if st.DemoDeployed {
		fmt.Println("  demo       counter deployed — next steps recapped below")
	}

	// The managed checkout is scratch space, so point teardown at a command
	// that stands on its own. A checkout the user supplied is still where
	// they left it, and the pasted `cd` is quoted — a space in the path would
	// otherwise land it somewhere else; the prose mentions read better
	// unquoted.
	section("Source tree")
	teardown := b.TeardownCommand(st, "")
	switch {
	case b.Managed && cleaned:
		note("The substrate tree was fetched to build your images and has been removed;",
			"re-running the installer fetches it again. Develop against your own clone.")
	case b.Managed:
		note("The substrate tree, if fetched, is cached at "+b.Root+";",
			"it is removed once a real install succeeds. Develop against your own clone.")
	default:
		note("Your substrate checkout at " + b.Root + " is untouched.")
		teardown = b.TeardownCommand(st, b.Root)
	}

	// Two teardown depths: delete only the control plane (keep the cluster),
	// or delete everything this install created and stop the charges. The
	// cleanup invocation carries the wizard's own answers so it can be run
	// from a fresh clone weeks later.
	section("Teardown, when you're done")
	note("Delete the Substrate control plane, keeping the cluster:")
	command(teardown)
	note("Delete everything this install created in GCP — the cluster, the",
		"snapshot bucket, IAM bindings, and dashboards:")
	command(cleanupCommand(st))

	// The wizard's "Next steps" panel vanishes with the alt screen, so a demo
	// install leaves a written copy behind.
	if st.DemoDeployed {
		section("Next steps — try the counter demo")
		portForward, demo := b.NextSteps(st)
		command(portForward)
		for _, cmd := range demo {
			command(cmd)
		}
	}
}

// cleanupCommand renders the tools/cleanup-gcp invocation for this install.
// Quoted for pasting, like the teardown command.
func cleanupCommand(st *state.Setup) string {
	return snapshot.CleanupCommand(st.ProjectID, st.ClusterName, st.Zone, st.BucketName)
}
