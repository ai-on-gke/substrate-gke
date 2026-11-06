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
	"regexp"
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
	if err := run(out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run walks the dry-run wizard and writes one capture per screen into out.
// On an error it removes the captures it wrote, so a failed run leaves no
// half-set behind in docs/screenshots.
func run(out string) (err error) {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	// Captures are piped, not shown on a terminal; without this lipgloss
	// would detect no TTY and strip every color from the frames.
	lipgloss.SetColorProfile(termenv.TrueColor)

	tmp, err := os.MkdirTemp("", "uishots")
	if err != nil {
		return err
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

	w := &walk{app: ui.NewApp(deps), out: out}
	defer func() {
		if err != nil {
			for _, p := range w.written {
				os.Remove(p)
			}
		}
	}()
	if err := w.pump(tea.WindowSizeMsg{Width: 110, Height: 26}); err != nil {
		return err
	}
	for _, m := range runCmd(w.app.Init()) {
		if err := w.pump(m); err != nil {
			return err
		}
	}

	if err := w.shot("welcome", state.Welcome, ""); err != nil {
		return err
	}

	w.press("enter")      // welcome → doctor
	w.press("enter")      // doctor → images
	w.press("1", "enter") // pre-built images
	// The pre-built track offers the pin's published release.
	w.press("enter", "enter", "enter")
	w.press("enter", "enter", "enter") // project fields
	// The list, not one of the cluster step's panels: the create row is
	// shown only while the list is.
	if err := w.shot("clusters", state.Cluster, "Create a new cluster"); err != nil {
		return err
	}

	w.press("5", "enter") // the installed fixture → blocked panel
	if err := w.shot("guard", state.Cluster, "already runs Substrate"); err != nil {
		return err
	}

	w.press("esc")        // back to the list
	w.press("1", "enter") // the clean cluster → provision runs (dry)
	if err := w.shot("provision", state.Provision, ""); err != nil {
		return err
	}

	w.press("enter")      // provision → ingress clients
	w.press("enter")      // the demo drivers' IDs → control plane
	w.press("enter")      // control plane → token exchange
	w.press("s")          // skip token exchange → filestore CSI
	w.press("s")          // skip filestore
	w.press("s")          // skip autoscaling → sandbox runtime
	w.press("enter")      // gVisor is the default choice → demo
	w.press("1", "enter") // deploy the counter demo
	w.press("enter")      // → complete
	if err := w.shot("complete", state.Complete, ""); err != nil {
		return err
	}
	return w.err
}

// walk drives one wizard. The first error stops every later step, so the
// key sequence reads straight through and the error names where it broke.
type walk struct {
	app     *ui.App
	out     string
	written []string
	err     error
}

// shot writes the screen the wizard is on, sized to show it whole, and
// refuses to write any other: the walk above is a hand-copied key sequence,
// and when a wizard change stalled it the tool wrote five copies of the
// screen it was stuck on without a word (#86). step is the step the wizard
// must be on; marker, when set, is text the wanted screen shows and other
// screens of that step do not.
func (w *walk) shot(name string, step state.Step, marker string) error {
	if w.err != nil {
		return w.err
	}
	if got := w.app.Step(); got != step {
		return fmt.Errorf("%s: the wizard is on %v, not %v\n\n%s", name, got, step, plain(w.app.View()))
	}
	if marker != "" && !strings.Contains(plain(w.app.View()), marker) {
		return fmt.Errorf("%s: the wizard is not on that screen (no %q in the frame)\n\n%s", name, marker, plain(w.app.View()))
	}
	if err := w.fit(name); err != nil {
		return err
	}
	path := filepath.Join(w.out, name+".ans")
	if err := os.WriteFile(path, []byte(w.app.View()), 0o644); err != nil {
		return err
	}
	w.written = append(w.written, path)
	fmt.Println("wrote", path)
	return nil
}

// fit sizes the frame to the screen on it. The installer clamps a screen to
// the terminal, so a frame one row short cuts off a panel's bottom border;
// a hand-tuned height did so silently after the next wording change, which
// is how a cropped guard screenshot was committed. The screen is rendered
// tall, measured, and given its rows plus one blank row above the key bar,
// which is then checked for.
func (w *walk) fit(name string) error {
	w.resize(200)
	tall := strings.Split(w.app.View(), "\n")
	content := tall[:len(tall)-2] // above the key bar's separator
	last := len(content) - 1
	for last > 0 && strings.TrimSpace(plain(content[last])) == "" {
		last--
	}
	want := nonBlank(content[:last+1])
	// The body sits one row short of the frame (see App.View), so the first
	// guess can land a row low; a frame that is a row too tall only shows a
	// second blank row, so the search goes up.
	for h := last + 4; h <= last+8; h++ {
		w.resize(h)
		lines := strings.Split(w.app.View(), "\n")
		body := lines[:len(lines)-2]
		if nonBlank(body) == want && strings.TrimSpace(plain(body[len(body)-1])) == "" {
			return w.err
		}
	}
	return fmt.Errorf("%s: no frame height shows the screen whole with a blank row above the key bar", name)
}

func nonBlank(lines []string) int {
	n := 0
	for _, l := range lines {
		if strings.TrimSpace(plain(l)) != "" {
			n++
		}
	}
	return n
}

// resize sets the frame to the README's 110 columns and h rows.
func (w *walk) resize(h int) {
	w.pump(tea.WindowSizeMsg{Width: 110, Height: h})
}

// plain strips the colors and the terminal hyperlinks from a frame.
var ansiSeq = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)")

func plain(s string) string { return ansiSeq.ReplaceAllString(s, "") }

func (w *walk) press(keys ...string) {
	for _, k := range keys {
		w.pump(key(k))
	}
}

// paste delivers text as one pasted message, as the UI tests' typeText does.
func (w *walk) paste(text string) {
	w.pump(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text), Paste: true})
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
	}
	panic("unknown key " + s)
}

// pump runs the model synchronously, executing every returned command
// inline until the queue drains — the UI tests' pump, minus testing.T. A
// model that never settles is recorded as the walk's error.
func (w *walk) pump(first tea.Msg) error {
	if w.err != nil {
		return w.err
	}
	deadline := time.Now().Add(30 * time.Second)
	for queue := []tea.Msg{first}; len(queue) > 0; {
		if time.Now().After(deadline) {
			w.err = fmt.Errorf("the wizard did not settle within 30s of %T", first)
			return w.err
		}
		msg := queue[0]
		queue = queue[1:]
		_, cmd := w.app.Update(msg)
		queue = append(queue, runCmd(cmd)...)
	}
	return nil
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
