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

// Package gkeoverlay adds this repository's GKE kustomize content (deploy/gke)
// to a fetched agent-substrate/substrate tree, so `ate-setup deploy ate-system`
// in that tree renders the control plane with it.
//
// Upstream is moving its GKE-specific manifests (the atelet ComputeClass
// toleration, the managed OpenTelemetry collector endpoint and the router
// PodMonitoring) out of manifests/ate-install and into this repository. A
// tree without them has to get them from deploy/gke.
//
// TODO: This is a stopgap until upstream ships `ate-setup deploy --component`
// (agent-substrate/substrate PR 3179) or `ate-setup render`
// (agent-substrate/substrate#3240). Once a pinned release has either, pass
// `--component deploy/gke/components/gke` (and apply deploy/gke/resources)
// instead of editing the tree, and delete this package.
package gkeoverlay

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// Dir is where the GKE kustomize content lives, relative to this
// repository's root.
const Dir = "deploy/gke"

// Paths inside Dir.
const (
	// componentSrc is the kustomize Component with GKE's patches.
	componentSrc = "components/gke"
	// resourcesSrc holds plain resources (the router PodMonitoring). They
	// are kept out of the Component because ate-setup's deploy --component
	// rejects components that add resources.
	resourcesSrc = "resources"
)

// Paths inside a Substrate tree.
const (
	installDir = "manifests/ate-install"

	// CoreMonitoring is the router PodMonitoring upstream ships while its
	// tree still carries the GKE content. Its absence is the signal that
	// the tree needs deploy/gke, in the same way the installer probes other
	// features by file presence (see snapshot.counterMicroVMDemoDir).
	CoreMonitoring = installDir + "/atenet-router-monitoring.yaml"

	// BaseKustomization is the kustomization `ate-setup deploy ate-system`
	// renders for a GKE install with the Envoy router, the only router the
	// installer deploys.
	BaseKustomization = installDir + "/base/kustomization.yaml"

	// ComponentDir and ResourcesDir are where the copies go. Upstream has
	// nothing named substrate-gke, so they cannot collide with its files.
	// They sit inside the tree because kustomize refuses absolute resource
	// and component paths, and a relative path out of the tree would break
	// when the tree moves. ate-setup renders with the load restrictor off,
	// which is what already lets base/ list ../atelet.yaml.
	ComponentDir = installDir + "/components/substrate-gke"
	ResourcesDir = installDir + "/substrate-gke"
)

// Entries added to BaseKustomization, relative to its directory.
const (
	componentEntry = "../components/substrate-gke"
	resourcesEntry = "../substrate-gke/"
)

// LinePrefix starts every line the add and check commands print. The deploy
// checklist keys its overlay row off it.
const LinePrefix = "substrate-gke GKE overlay: "

// Outcome is what Add did.
type Outcome int

const (
	// NotNeeded: the tree still ships upstream's GKE content; nothing was
	// touched. Adding deploy/gke there would duplicate the PodMonitoring and
	// the toleration.
	NotNeeded Outcome = iota
	// Added: at least one file was written or the kustomization was edited.
	Added
	// AlreadyPresent: an earlier run left the tree exactly as Add would.
	AlreadyPresent
)

// Find returns the absolute path of this repository's deploy/gke. An explicit
// path wins and must hold it. Otherwise each start directory and its
// ancestors are searched, which covers `go run .` from installer/ and the
// binary `make build` writes to bin/.
func Find(explicit string, starts ...string) (string, error) {
	if explicit != "" {
		if err := check(explicit); err != nil {
			return "", err
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
			candidate := filepath.Join(dir, Dir)
			if check(candidate) == nil {
				return candidate, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("cannot find %s above %s; run the installer from a substrate-gke checkout or pass --gke-overlay",
		Dir, strings.Join(starts, " or "))
}

// check reports whether dir holds the deploy/gke layout Add copies from.
func check(dir string) error {
	component := filepath.Join(dir, componentSrc, "kustomization.yaml")
	if _, err := os.Stat(component); err != nil {
		return fmt.Errorf("%s is not substrate-gke's %s: %w", dir, Dir, err)
	}
	if files, err := resourceFiles(dir); err != nil {
		return fmt.Errorf("%s is not substrate-gke's %s: %w", dir, Dir, err)
	} else if len(files) == 0 {
		return fmt.Errorf("%s is not substrate-gke's %s: %s has no .yaml files", dir, Dir, resourcesSrc)
	}
	return nil
}

// resourceFiles lists the .yaml files directly under overlay's resources
// directory, sorted, so the kustomization entries come out in a stable order.
func resourceFiles(overlay string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(overlay, resourcesSrc))
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			files = append(files, e.Name())
		}
	}
	slices.Sort(files)
	return files, nil
}

// Needed reports whether the tree at root lacks upstream's GKE content, and
// so needs deploy/gke. An error means the tree has no base kustomization to
// add it to.
func Needed(root string) (bool, error) {
	if _, err := os.Stat(filepath.Join(root, CoreMonitoring)); err == nil {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(root, BaseKustomization)); err != nil {
		return false, fmt.Errorf("%s has neither %s nor %s; it does not look like a substrate tree", root, CoreMonitoring, BaseKustomization)
	}
	return true, nil
}

// Add copies overlay's Component and resources into the Substrate tree at
// root and lists them in its base kustomization, if the tree needs them (see
// Needed). It is idempotent: a second run writes nothing and adds no
// duplicate entries. Every file is replaced by rename, and the result
// depends only on overlay, so two runs adding to one tree at once leave it
// whole.
func Add(root, overlay string) (Outcome, error) {
	needed, err := Needed(root)
	if err != nil || !needed {
		return NotNeeded, err
	}
	if err := check(overlay); err != nil {
		return NotNeeded, err
	}
	changed, err := syncDir(filepath.Join(overlay, componentSrc), filepath.Join(root, ComponentDir))
	if err != nil {
		return NotNeeded, err
	}
	resChanged, err := syncDir(filepath.Join(overlay, resourcesSrc), filepath.Join(root, ResourcesDir))
	if err != nil {
		return NotNeeded, err
	}
	files, err := resourceFiles(overlay)
	if err != nil {
		return NotNeeded, err
	}
	var resources []string
	for _, f := range files {
		resources = append(resources, resourcesEntry+f)
	}
	kChanged, err := editKustomization(filepath.Join(root, BaseKustomization), componentEntry, resources)
	if err != nil {
		return NotNeeded, err
	}
	if changed || resChanged || kChanged {
		return Added, nil
	}
	return AlreadyPresent, nil
}

// syncDir makes dst's regular files match src's, recursively: it writes each
// file whose content differs and removes files src does not have. It reports
// whether anything changed.
func syncDir(src, dst string) (bool, error) {
	changed := false
	want := map[string]bool{}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		want[rel] = true
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		wrote, err := writeIfChanged(filepath.Join(dst, rel), data)
		changed = changed || wrote
		return err
	})
	if err != nil {
		return false, err
	}
	err = filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dst, path)
		if err != nil {
			return err
		}
		// A concurrent run's temporary file is not stale; removing it would
		// fail that run's rename.
		if !want[rel] && !isTemp(d.Name()) {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			changed = true
		}
		return nil
	})
	return changed, err
}

// tempInfix marks the temporary files writeIfChanged renames into place.
const tempInfix = ".tmp-"

func isTemp(name string) bool {
	return strings.HasPrefix(name, ".") && strings.Contains(name, tempInfix)
}

// writeIfChanged writes data to path through a temporary file and a rename,
// unless path already holds exactly data. It reports whether it wrote.
func writeIfChanged(path string, data []byte) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+tempInfix)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}

// editKustomization adds component under `components:` and each of resources
// under `resources:` in the kustomization at path, skipping entries already
// there, and drops entries under resourcesEntry that resources no longer
// names, which an older deploy/gke may have added. It edits the YAML tree
// rather than the text, so comments and the other entries survive. It
// reports whether it wrote the file.
func editKustomization(path, component string, resources []string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	node, err := yaml.Parse(string(data))
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	added := false
	for _, list := range []struct {
		field   string
		entries []string
	}{
		{"resources", resources},
		{"components", []string{component}},
	} {
		seq, err := node.Pipe(yaml.LookupCreate(yaml.SequenceNode, list.field))
		if err != nil {
			return false, fmt.Errorf("%s: %s: %w", path, list.field, err)
		}
		var values []string
		kept := seq.Content()[:0]
		for _, el := range seq.Content() {
			if list.field == "resources" && strings.HasPrefix(el.Value, resourcesEntry) && !slices.Contains(list.entries, el.Value) {
				added = true
				continue
			}
			kept = append(kept, el)
			values = append(values, el.Value)
		}
		seq.YNode().Content = kept
		for _, entry := range list.entries {
			if slices.Contains(values, entry) {
				continue
			}
			if err := seq.PipeE(yaml.Append(yaml.NewScalarRNode(entry).YNode())); err != nil {
				return false, fmt.Errorf("%s: add %s entry %s: %w", path, list.field, entry, err)
			}
			values = append(values, entry)
			added = true
		}
	}
	if !added {
		return false, nil
	}
	out, err := node.String()
	if err != nil {
		return false, fmt.Errorf("render %s: %w", path, err)
	}
	return writeIfChanged(path, []byte(out))
}

// HideFromGit keeps the overlay out of `git status` in an installer-fetched
// tree: Go stamps binaries built from a tree with changes as modified, which
// would mark every image a build from source pushes, and the ateapi
// --version the upgrade track reads, as -dirty. The copies are listed in
// .git/info/exclude, the way the fetch excludes its completion marker, and
// the edited kustomization is marked skip-worktree.
//
// It must not run on a checkout the user supplied: those flags would hide
// the edit from the user's own git.
func HideFromGit(root string) error {
	exclude := filepath.Join(root, ".git", "info", "exclude")
	cur, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	lines := strings.Split(string(cur), "\n")
	var add []string
	for _, p := range []string{"/" + ComponentDir + "/", "/" + ResourcesDir + "/"} {
		if !slices.Contains(lines, p) {
			add = append(add, p)
		}
	}
	if len(add) > 0 {
		text := string(cur)
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += strings.Join(add, "\n") + "\n"
		if _, err := writeIfChanged(exclude, []byte(text)); err != nil {
			return err
		}
	}
	// `git ls-files -v` tags a skip-worktree entry with S.
	out, err := exec.Command("git", "-C", root, "ls-files", "-v", "--", BaseKustomization).Output()
	if err != nil {
		return fmt.Errorf("git ls-files %s: %w", BaseKustomization, err)
	}
	if strings.HasPrefix(string(out), "S ") {
		return nil
	}
	if out, err := exec.Command("git", "-C", root, "update-index", "--skip-worktree", "--", BaseKustomization).CombinedOutput(); err != nil {
		return fmt.Errorf("git update-index --skip-worktree %s: %w: %s", BaseKustomization, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Messages Apply and Check print, after LinePrefix.
const (
	msgNotNeeded = "not needed; this substrate tree still ships its own GKE manifests (" + CoreMonitoring + ")"
	msgAdded     = "added " + Dir + " to " + BaseKustomization + " (" + ComponentDir + ", " + ResourcesDir + ")"
	msgPresent   = Dir + " is already in " + BaseKustomization
)

// NotNeededLine is what Apply and Check print for a tree that still ships its
// own GKE manifests, which every pinned release does today. The dry run
// replays it.
const NotNeededLine = LinePrefix + msgNotNeeded

// Apply adds overlay to the tree at root (see Add) and prints the outcome to
// w. With hide, which is for trees the installer fetched itself, it also
// keeps the change out of git's view (see HideFromGit); failing that only
// costs a -dirty build stamp, so it is reported and not returned.
func Apply(w io.Writer, root, overlay string, hide bool) error {
	outcome, err := Add(root, overlay)
	if err != nil {
		return fmt.Errorf("adding %s to %s: %w", Dir, root, err)
	}
	switch outcome {
	case NotNeeded:
		fmt.Fprintln(w, LinePrefix+msgNotNeeded)
		return nil
	case Added:
		fmt.Fprintln(w, LinePrefix+msgAdded)
	case AlreadyPresent:
		fmt.Fprintln(w, LinePrefix+msgPresent)
	}
	if !hide {
		fmt.Fprintln(w, LinePrefix+"the tree at "+root+" now has local changes; `git checkout -- "+BaseKustomization+"` and removing "+ComponentDir+" and "+ResourcesDir+" undo them")
		return nil
	}
	if err := HideFromGit(root); err != nil {
		fmt.Fprintln(w, LinePrefix+"warning: could not hide the change from git, so binaries built from this tree are stamped -dirty: "+err.Error())
	}
	return nil
}

// Check prints to w whether the tree at root needs deploy/gke, without
// changing it. The installer runs it instead of Apply on a checkout the user
// supplied, which it never modifies. addCommand is how the user adds the
// overlay themselves.
func Check(w io.Writer, root, addCommand string) error {
	needed, err := Needed(root)
	if err != nil {
		return err
	}
	switch {
	case !needed:
		fmt.Fprintln(w, LinePrefix+msgNotNeeded)
	case hasEntry(root):
		fmt.Fprintln(w, LinePrefix+msgPresent)
	default:
		fmt.Fprintln(w, LinePrefix+"warning: not added to "+root+", a checkout you supplied, which the installer does not modify. "+
			"It lacks substrate's GKE manifests (the atelet ComputeClass toleration, the managed OpenTelemetry endpoint and the router PodMonitoring), "+
			"so this deploy runs without them. To add them, run: "+addCommand)
	}
	return nil
}

// hasEntry reports whether the tree's base kustomization already lists the
// Component, as after an earlier Apply.
func hasEntry(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, BaseKustomization))
	return err == nil && strings.Contains(string(data), componentEntry)
}
