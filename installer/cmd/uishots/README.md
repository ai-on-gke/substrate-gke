# uishots

Generates the screenshots embedded in the repository's main README by driving
the dry-run wizard headlessly and capturing its screens — so the images
regenerate from the current code instead of rotting like hand-taken ones.

## Regenerating the screenshots

One-time setup:

```bash
go install github.com/charmbracelet/freeze@latest   # renders the captures
```

Then, from the repository root:

```bash
make screenshots
```

That runs this tool to write one ANSI capture (`.ans`) per screen into
`docs/screenshots/`, renders each to an SVG with `freeze --window`, and
removes the captures. Commit the changed SVGs. Run it whenever a UI change
makes the README's images stale.

## How it works

The tool builds the wizard with the same dry-run dependencies the UI tests
use (simulated runner, canned gcloud data), feeds it synthetic key presses
through the tests' pump loop, and writes `app.View()` at five points:
`welcome`, `clusters` (install-state badges), `guard` (the reinstall block),
`provision`, and `complete`. Colors are forced with
`lipgloss.SetColorProfile(termenv.TrueColor)` since there is no terminal to
detect.

## Adding or changing a screen

Add a `press(...)` sequence to walk to the screen and a `shot(out, "name",
app, marker)` call in `main.go`, then reference `docs/screenshots/name.svg`
from the README. `marker` is text that screen shows and no other; `shot`
exits with an error when the frame lacks it, so a wizard change that stalls
the walk fails `make screenshots` instead of writing the wrong screen five
times. Frames are 110 columns wide and as tall as the screen needs: `shot`
measures the screen and leaves one blank row above the key bar, so there is
no height to tune when a panel grows.
