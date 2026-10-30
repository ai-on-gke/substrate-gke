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

// Command uishots renders the README's screenshots: it drives the dry-run
// wizard headlessly, the way the UI tests do, and writes one ANSI capture
// per screen. `make screenshots` turns the captures into PNGs with freeze.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/ai-on-gke/substrate-gke/installer/internal/doctor"
	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/gcp"
	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
	"github.com/ai-on-gke/substrate-gke/installer/internal/ui"
)

func main() {
	out := "docs/screenshots"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		fatal(err)
	}
	// Captures are piped, not shown on a terminal; without this lipgloss
	// would detect no TTY and strip every color from the frames.
	lipgloss.SetColorProfile(termenv.TrueColor)

	tmp, err := os.MkdirTemp("", "uishots")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(tmp)

	deps := &ui.Deps{
		Setup:   state.NewSetup(),
		Runner:  execx.DryRun{Delay: time.Millisecond},
		GCP:     &gcp.Client{DryRun: true},
		Builder: snapshot.NewBuilder(tmp, false),
		Checks:  doctor.Checks(tmp, true),
		DryRun:  true,
		LogPath: "~/.cache/substrate-gke/logs/installer.log",
	}
	// A demo run, so the completion screen shows the demo's next steps.
	deps.Setup.DemoMode = true
	// Bootstrap's command line shows the resolved setup-gcp directory. Use
	// install.sh's default clone location ($HOME/.substrate-gke) with a
	// placeholder home directory, so the screenshot shows neither this
	// machine's path nor an unexpandable '~' (ShellQuote single-quotes it).
	deps.Builder.SetupGCP = "/home/you/.substrate-gke/tools/setup-gcp"
	// Provision's checklist names the substrate checkout, which would
	// otherwise be the random temporary directory above and change the
	// screenshot on every run.
	deps.Builder.Root = "/home/you/substrate"
	app := ui.NewApp(deps)
	pump(app, tea.WindowSizeMsg{Width: 110, Height: 26})
	for _, m := range runCmd(app.Init()) {
		pump(app, m)
	}

	// Each screen gets the frame height that shows it whole: the installer
	// clamps a screen to the terminal, so a short frame cuts off its panels.
	resize(app, 31)
	shot(out, "welcome", app)
	resize(app, 26)

	press(app, "enter")      // welcome → doctor
	press(app, "enter")      // doctor → images
	press(app, "1", "enter") // pre-built images
	// The pre-built track offers the pin's published release.
	press(app, "enter", "enter", "enter")
	press(app, "enter", "enter", "enter") // project fields
	shot(out, "clusters", app)            // list with install-state badges

	press(app, "5", "enter") // the installed fixture → blocked panel
	resize(app, 43)
	shot(out, "guard", app)
	resize(app, 26)

	press(app, "esc")        // back to the list
	press(app, "1", "enter") // the clean cluster → provision runs (dry)
	shot(out, "provision", app)

	press(app, "enter")      // provision → ingress clients
	press(app, "enter")      // the demo drivers' IDs → control plane
	press(app, "enter")      // control plane → filestore CSI
	press(app, "s")          // skip filestore
	press(app, "s")          // skip autoscaling → sandbox runtime
	press(app, "enter")      // gVisor is the default choice → demo
	press(app, "1", "enter") // deploy the counter demo
	press(app, "enter")      // → complete
	resize(app, 36)
	shot(out, "complete", app)
}

// resize sets the frame to the README's 110 columns and h rows.
func resize(app *ui.App, h int) {
	pump(app, tea.WindowSizeMsg{Width: 110, Height: h})
}

func shot(dir, name string, app *ui.App) {
	path := filepath.Join(dir, name+".ans")
	if err := os.WriteFile(path, []byte(app.View()), 0o644); err != nil {
		fatal(err)
	}
	fmt.Println("wrote", path)
}

func press(app *ui.App, keys ...string) {
	for _, k := range keys {
		pump(app, key(k))
	}
}

// paste delivers text as one pasted message, as the UI tests' typeText does.
func paste(app *ui.App, text string) {
	pump(app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text), Paste: true})
}

func key(s string) tea.Msg {
	if len(s) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	default:
		fatal(fmt.Errorf("unknown key %q", s))
		return nil
	}
}

// pump runs the model synchronously, executing every returned command
// inline until the queue drains — the UI tests' pump, minus testing.T.
func pump(app *ui.App, first tea.Msg) {
	deadline := time.Now().Add(30 * time.Second)
	for queue := []tea.Msg{first}; len(queue) > 0; {
		if time.Now().After(deadline) {
			fatal(fmt.Errorf("pump did not converge"))
		}
		msg := queue[0]
		queue = queue[1:]
		_, cmd := app.Update(msg)
		queue = append(queue, runCmd(cmd)...)
	}
}

func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, runCmd(c)...)
		}
		return out
	}
	if strings.Contains(fmt.Sprintf("%T", msg), "BlinkMsg") {
		return nil
	}
	return []tea.Msg{msg}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
