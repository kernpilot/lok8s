package kubeone

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/ui"
)

// runPrefix starts the name of the private run directory in the work dir
// (and of the manifest copy beside it).
const runPrefix = ".kubeone-run-"

// stageRun makes the copy of the run that KubeOne reads (bash:
// kubeone::_stage_run). KubeOne applies each file in the addons directory
// of the manifest (addons.path, relative to the manifest) at its "ensure
// custom addons" step, and it reads the files then. The files in the work
// dir are shared: another lo run, or another writer, can replace them
// after lo checked them. So lo reads each file once, checks it with the
// keys-only apply guard (build.ReadCheckedFile), and writes the checked
// bytes into a new private run directory <workDir>/.kubeone-run-<random>
// (mode 0700, files 0600). A file with a keys-only Secret, or one that
// cannot be read or does not parse, stops the apply.
//
// The manifest copy <run>.yaml (0600) is the manifest that lo read once,
// with addons.path set to the run's addons directory. It is beside the
// run directory, in the work dir, because KubeOne resolves the other
// relative paths of the manifest (the CA file, the audit and node-selector
// files, the PKI backup <cluster>.tar.gz) against the directory of the
// manifest: these do not change.
//
// It returns the manifest for kubeone (relative to workDir) and the
// cleanup that removes the run directory and the copy. A manifest that
// names no addons directory runs as it is (kubeone.yaml); an addons path
// that is not a directory gets no copy, and kubeone reports it.
func stageRun(stderr io.Writer, workDir string) (string, func(), error) {
	noop := func() {}
	manifest := filepath.Join(workDir, "kubeone.yaml")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		ui.ErrorTo(stderr, "kubeone: cannot read %s", manifest)
		return "", noop, ui.Handled(fmt.Errorf("kubeone: read %s: %w", manifest, err))
	}
	// A decode into a struct resolves the aliases and merge keys of the
	// manifest, as KubeOne's own decode does.
	var spec struct {
		Addons struct {
			Path string `yaml:"path"`
		} `yaml:"addons"`
	}
	root, err := parseYAMLDoc(raw)
	if err == nil {
		err = yaml.Unmarshal(raw, &spec)
	}
	if err != nil {
		ui.ErrorTo(stderr, "kubeone: %s does not parse as YAML", manifest)
		return "", noop, ui.Handled(fmt.Errorf("kubeone: parse %s: %w", manifest, err))
	}
	if spec.Addons.Path == "" {
		return "kubeone.yaml", noop, nil
	}
	src := spec.Addons.Path
	if !filepath.IsAbs(src) {
		abs, err := filepath.Abs(workDir)
		if err != nil {
			return "", noop, fmt.Errorf("kubeone: work dir: %w", err)
		}
		src = filepath.Join(abs, src)
	}

	run, err := os.MkdirTemp(workDir, runPrefix)
	if err != nil {
		ui.ErrorTo(stderr, "kubeone: cannot make a run directory in %s", workDir)
		return "", noop, ui.Handled(fmt.Errorf("kubeone: run directory: %w", err))
	}
	if run, err = filepath.Abs(run); err != nil {
		return "", noop, fmt.Errorf("kubeone: run directory: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(run)
		_ = os.Remove(run + ".yaml")
	}
	if err := copyCheckedAddons(stderr, src, filepath.Clean(spec.Addons.Path), filepath.Join(run, "addons")); err != nil {
		cleanup()
		return "", noop, err
	}
	addons := mapValue(root, "addons")
	if addons != nil && addons.Kind == yaml.AliasNode && addons.Alias != nil {
		// Edit a copy of the anchored mapping, not the anchor.
		own := *addons.Alias
		own.Anchor = ""
		own.Content = append([]*yaml.Node(nil), addons.Alias.Content...)
		addons = &own
		setKey(root, "addons", addons)
	}
	setKey(ensureMapPath(root, "addons"), "path", strNode(filepath.Join(run, "addons")))
	if err := writeYAMLExclusive(run+".yaml", root); err != nil {
		cleanup()
		ui.ErrorTo(stderr, "kubeone: cannot write the manifest copy %s.yaml", run)
		return "", noop, ui.Handled(fmt.Errorf("kubeone: manifest copy: %w", err))
	}
	return filepath.Base(run) + ".yaml", cleanup, nil
}

// copyCheckedAddons copies the directory src into the new directory dst:
// each directory with mode 0700, and each file with the bytes that passed
// the keys-only apply guard, with mode 0600. what names a file in the
// guard's message: shown (the addons path of the manifest) joined with
// the path of the file in src. A directory that cannot be read is
// refused as a file that cannot be read. A src that is not a directory
// gets no copy.
func copyCheckedAddons(stderr io.Writer, src, shown, dst string) error {
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return nil
	}
	root, err := filepath.EvalSymlinks(src)
	if err != nil {
		return nil
	}
	return filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if err == nil && de.IsDir() {
			return os.Mkdir(target, 0o700)
		}
		// A file (a symbolic link too: lo reads what it points at), or
		// a directory that cannot be read: the guard refuses the latter.
		checked, cerr := build.ReadCheckedFile(stderr, "kubeone", filepath.Join(shown, rel), path)
		if cerr != nil {
			return cerr
		}
		if werr := writeExclusive(target, checked); werr != nil {
			ui.ErrorTo(stderr, "kubeone: cannot write the run copy of %s", filepath.Join(shown, rel))
			return ui.Handled(fmt.Errorf("kubeone: run copy: %w", werr))
		}
		return nil
	})
}

// writeExclusive writes data to a new file with mode 0600: it fails when
// the file exists.
func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// writeYAMLExclusive is saveYAMLDoc into a new file (writeExclusive).
func writeYAMLExclusive(path string, root *yaml.Node) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return writeExclusive(path, buf.Bytes())
}
