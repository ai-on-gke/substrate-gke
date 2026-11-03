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
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"

	"github.com/ai-on-gke/substrate-gke/installer/internal/doctor"
	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/gcp"
	"github.com/ai-on-gke/substrate-gke/installer/internal/gkeoverlay"
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
		setupGCP      = flag.String("setup-gcp", "", "path to this repository's tools/setup-gcp (default: found above the working directory or the executable)")
		gkeOverlay    = flag.String("gke-overlay", "", "path to this repository's deploy/gke (default: found above the working directory or the executable)")
		addOverlay    = flag.String("add-gke-overlay", "", "add deploy/gke to the substrate tree in this directory if it lacks upstream's GKE manifests, and exit")
		checkOverlay  = flag.String("check-gke-overlay", "", "report whether the substrate tree in this directory needs deploy/gke, without changing it, and exit")
		demo          = flag.Bool("demo", false, "demo install: end with a counter demo, and allow its driver pods on the ingress router (make run-demo)")
	)
	flag.Parse()

	// setup-gcp and deploy/gke are found relative to this repository's
	// checkout: above the working directory (`go run .` from installer/) or
	// the executable (`make build` writes bin/).
	cwd, _ := os.Getwd()
	exeDir := ""
	exe, exeErr := os.Executable()
	if exeErr == nil {
		exeDir = filepath.Dir(exe)
	}
	findOverlay := func() string {
		dir, err := gkeoverlay.Find(*gkeOverlay, cwd, exeDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return dir
	}

	// The install and upgrade steps run these two in the fetched tree before
	// ate-setup reads it. See gkeoverlay for why, and for when they go away.
	if *addOverlay != "" {
		// Hide the change from git only in a tree the installer fetched,
		// never in a checkout of the user's.
		hide := snapshot.Fetched(*addOverlay, true)
		if err := gkeoverlay.Apply(os.Stdout, absPath(*addOverlay), findOverlay(), hide); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if *checkOverlay != "" {
		tree := absPath(*checkOverlay)
		// deploy/gke sits two levels below the repository root.
		installerDir := filepath.Join(filepath.Dir(filepath.Dir(findOverlay())), "installer")
		add := "(cd " + snapshot.ShellQuote(installerDir) + " && go run . --add-gke-overlay " + snapshot.ShellQuote(tree) + ")"
		if err := gkeoverlay.Check(os.Stdout, tree, add); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if *fetchTree != "" {
		if err := snapshot.FetchTree(context.Background(), *fetchTree, *fetchCommit); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		// Upstream's runbook deploys from this tree, so it gets deploy/gke
		// the way the install's own tree does.
		if err := gkeoverlay.Apply(os.Stdout, absPath(*fetchTree), findOverlay(), true); err != nil {
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
		fatal, sourceOnly := doctor.RunCLI(context.Background(), doctor.Checks(root, managed))
		if fatal > 0 {
			fmt.Printf("\n%d fatal problem(s) found.\n", fatal)
			os.Exit(1)
		}
		if sourceOnly > 0 {
			fmt.Printf("\nReady to install pre-built images. Fix the %d Docker problem(s) above\nbefore building Substrate 0.2 or later from source.\n", sourceOnly)
			return
		}
		fmt.Println("\nAll good — run the installer.")
		return
	}

	// Everything from here on is the wizard, a Bubble Tea program. Piped or
	// redirected (`go run . | tee log`, a CI step) it never learns its window
	// size and repaints "loading…" forever, so refuse up front and say why.
	if msg := needsTerminal(os.Stdin, os.Stdout); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(2)
	}

	// Bootstrap runs setup-gcp from this repository, and the deploy adds
	// deploy/gke by re-running this executable. Resolve all three before the
	// wizard starts, so a missing one fails here rather than mid-install.
	setupGCPDir, err := snapshot.FindSetupGCP(*setupGCP, cwd, exeDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	overlayDir := findOverlay()
	if exeErr != nil {
		fmt.Fprintln(os.Stderr, "error: cannot locate the installer's own executable, which the deploy step runs to add deploy/gke:", exeErr)
		os.Exit(1)
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
	deps.Builder.SetupGCP = setupGCPDir
	deps.Builder.GKEOverlay = overlayDir
	deps.Builder.Self = exe
	deps.Setup.DemoMode = *demo
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
	_, err = tea.NewProgram(app, tea.WithAltScreen()).Run()
	// Kill any gcloud the TUI left running (a token fetch, a probe) rather
	// than leave it orphaned; before the error check, since os.Exit skips
	// deferred calls.
	deps.GCP.Close()
	if err != nil {
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
		// A screen's own parting words: the reinstall guard's cleanup
		// command, which its panel's borders made awkward to copy.
		for _, n := range app.ExitNotes() {
			fmt.Println()
			if n.Lead != "" {
				fmt.Println(theme.Subtle.Render("  " + n.Lead))
			}
			if n.Command != "" {
				fmt.Println("  " + theme.CommandLine.Render(n.Command))
			}
			if n.Trail != "" {
				fmt.Println(theme.Subtle.Render("  " + n.Trail))
			}
		}
		if deps.LogPath != "" {
			fmt.Printf("\nDetailed command log written to:\n  %s\n", theme.Accent.Render(deps.LogPath))
		}
		return
	}
	if st.Upgrade {
		fmt.Println(theme.Good.Render("Upgrade prepared."))
		fmt.Println(b.UpgradeSummary(st))
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
	fmt.Printf("  project    %s\n  cluster    %s (%s)\n  sandbox    %s\n  bucket     gs://%s\n  images     %s\n",
		st.ProjectID, st.ClusterName, st.Zone, ui.SandboxSummary(st), st.BucketName, st.ImageSummary())
	if deps.LogPath != "" {
		fmt.Printf("  log        %s\n", deps.LogPath)
	}
	switch {
	case st.FilestoreCSIDeployed:
		fmt.Println("  filestore  CSI driver deployed (gcp-filestore-csi-driver)")
	case st.FilestoreCSIFailed:
		// Not "skipped": the run may have left an IAM grant or a partial
		// namespace behind, and the log says which (see FilestoreSummary).
		fmt.Println("  filestore  CSI driver install failed and was skipped; see the log, and check namespace gcp-filestore-csi-driver for a partial apply")
	}
	if st.AutoscaleEnabled {
		fmt.Printf("  autoscale  %s, %d–%d nodes\n", st.NodePool, st.AutoscaleMin, st.AutoscaleMax)
	}
	if st.DemoDeployed {
		fmt.Printf("  demo       %s — next steps recapped below\n", ui.DemoSummary(st))
	}

	// The managed checkout is scratch space, so point teardown at a command
	// that stands on its own. A checkout the user supplied is still where
	// they left it, and the pasted `cd` is quoted — a space in the path would
	// otherwise land it somewhere else; the prose mentions read better
	// unquoted.
	section("Source tree")
	teardown := b.TeardownCommand(st, "")
	// Why the tree was fetched depends on the image source: a pre-built
	// install builds nothing and used it for its deploy manifests only.
	fetchedFor := "to build your images"
	if st.Prebuilt() {
		fetchedFor = "for its deploy manifests"
	}
	switch {
	case b.Managed && cleaned:
		note("The substrate tree was fetched "+fetchedFor+" and has been removed;",
			"re-running the installer fetches it again. Develop against your own clone.")
	case b.Managed:
		note("The substrate tree, if fetched, is cached at "+b.Root+";",
			"it is removed once a real install succeeds. Develop against your own clone.")
	default:
		note("Your substrate checkout at " + b.Root + " is untouched.")
		teardown = b.TeardownCommand(st, b.Root)
	}

	// Three teardown depths: delete only the control plane (keep the
	// cluster); delete the cluster and what only it uses; or delete the
	// project-wide resources as well. The cleanup invocations carry the
	// wizard's own answers so they can be run from a fresh clone weeks later.
	//
	// The cluster-scoped form leads. The dashboards and the transfer and node
	// service accounts are shared by every Substrate cluster in the project,
	// and the installer cannot tell whether another one still depends on
	// them — the provision step finding them already there is one sign — so
	// the project-wide form is a second, explicit option.
	section("Teardown, when you're done")
	note("Delete the Substrate control plane, keeping the cluster:")
	command(teardown)
	resources := "snapshot and asset buckets, and the IAM bindings on them"
	if st.CreatesArtifactRepository() {
		resources = "snapshot and asset buckets, the IAM bindings on them, and the image repository"
	}
	note("Delete this cluster and what only it uses — the cluster, its", resources+":")
	command(b.CleanupCommand(st, true))
	// A Filestore run that failed may still have created the account and
	// its project role (#143), so it is named for a failed run too.
	accounts := "the snapshot transfer and node service accounts"
	if st.FilestoreCSIDeployed || st.FilestoreCSIFailed {
		accounts = "the snapshot transfer, Filestore CSI and node service accounts"
	}
	note("To also remove the project-wide resources every Substrate cluster in "+st.ProjectID+" shares —",
		"the dashboards and "+accounts+" — only if no",
		"other cluster uses them:")
	command(b.CleanupCommand(st, false))

	// The wizard's "Next steps" panel vanishes with the alt screen, so a demo
	// install leaves a written copy behind.
	if st.DemoDeployed {
		section("Next steps — try the " + ui.DemoName(st) + " demo")
		lead, demo := b.NextSteps(st)
		if lead != "" {
			command(lead)
		}
		for _, cmd := range demo {
			command(cmd)
		}
	}

	// The wizard's panel links to the router's documentation as a terminal
	// hyperlink, which not every terminal opens; the URL is printed here in
	// full, where it can be copied.
	section("Reaching actors through the ingress router")
	note(b.RouterNote(st))
}

// needsTerminal returns the refusal to print when the wizard cannot run
// because stdin or stdout is not a terminal, or "" when both are. The
// message names the log the installer writes, since a log is what piping
// the wizard through tee was after, and the issue that tracks a mode for
// automation, which this check is not a substitute for.
func needsTerminal(stdin, stdout *os.File) string {
	in, out := term.IsTerminal(stdin.Fd()), term.IsTerminal(stdout.Fd())
	if in && out {
		return ""
	}
	which := "input and output are"
	switch {
	case in:
		which = "output is"
	case out:
		which = "input is"
	}
	msg := "The installer is an interactive terminal program, but its " + which + " not a terminal. " +
		"Run it directly in a terminal rather than piping or redirecting it."
	if dir, err := execx.DefaultLogDir(); err == nil {
		msg += " To keep a record of a run, use the log it writes under " + dir + " (see README, Logs)."
	}
	return msg + " For automation see https://github.com/ai-on-gke/substrate-gke/issues/107."
}

// absPath returns path made absolute, or path itself if that fails.
func absPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
