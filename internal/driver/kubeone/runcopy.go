package kubeone

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/ui"
)

// runPrefix starts the name of the private run directory in the work dir
// (and of the manifest copy beside it).
const runPrefix = ".kubeone-run-"

// mkdirTemp makes the run directory (a variable, so a test can name it).
var mkdirTemp = os.MkdirTemp

// explodeBudget bounds the nodes of an exploded manifest: an alias that
// names a large node many times (an alias bomb) stops the apply.
const explodeBudget = 1 << 20

// stageRun makes the copy of the run that KubeOne reads (bash:
// kubeone::_stage_run). KubeOne reads its manifest when it starts, and it
// applies each file in the addons directory of the manifest (addons.path,
// relative to the manifest) at its "ensure custom addons" step, when it
// reads the files. The files in the work dir are shared: another lo run,
// or another writer, can replace them after lo read or checked them.
//
// So lo reads kubeone.yaml once and makes a new private run directory
// <workDir>/.kubeone-run-<random> (mode 0700). The manifest copy <run>.yaml
// (0600) is what kubeone reads. Without an addons path it holds the bytes
// that lo read, unchanged. With an addons path, lo reads each file of that
// directory once, checks it with the keys-only apply guard
// (build.ReadCheckedFile), and writes the checked bytes into <run>/addons
// (directories 0700, files 0600); the manifest copy is then the manifest
// with its aliases and merge keys resolved (as yq's explode(.) does) and
// addons.path set to <run>/addons. A file with a keys-only Secret, one that
// cannot be read or does not parse, and an addons path that cannot be
// read stop the apply. An addons path that does not exist, or that is not
// a directory, gets no copy: kubeone reports it. A manifest with more than
// one YAML document, and an addons.path that is not a string (false, a
// number, a mapping, a list), stop the apply too (bash: the same).
//
// The manifest copy is beside the run directory, in the work dir, because
// KubeOne resolves the other relative paths of the manifest (the CA file,
// the audit and node-selector files, the PKI backup <cluster>.tar.gz)
// against the directory of the manifest: these do not change.
//
// It returns the manifest for kubeone (relative to workDir) and the
// cleanup that removes the run directory and the manifest copy. On an
// error, it removes the run directory itself, and a manifest copy that it
// began to write (writeExclusive); a file that was at the path of the
// manifest copy already (it refuses to write there) stays.
func stageRun(stderr io.Writer, workDir string) (string, func(), error) {
	noop := func() {}
	manifest := filepath.Join(workDir, "kubeone.yaml")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		ui.ErrorTo(stderr, "kubeone: cannot read %s", manifest)
		return "", noop, ui.Handled(fmt.Errorf("kubeone: read %s: %w", manifest, err))
	}
	docs, err := countDocs(raw)
	var root *yaml.Node
	if err == nil {
		var doc *yaml.Node
		if doc, err = parseYAMLDoc(raw); err == nil {
			budget := explodeBudget
			root, err = explode(doc, &budget)
		}
	}
	if err != nil {
		ui.ErrorTo(stderr, "kubeone: %s does not parse as YAML", manifest)
		return "", noop, ui.Handled(fmt.Errorf("kubeone: parse %s: %w", manifest, err))
	}
	if docs > 1 {
		ui.ErrorTo(stderr, "kubeone: %s holds more than one YAML document", manifest)
		return "", noop, ui.Handled(fmt.Errorf("kubeone: %s: %d documents", manifest, docs))
	}
	path := ""
	if p := mapValue(mapValue(root, "addons"), "path"); p != nil && (p.Kind != yaml.ScalarNode || p.ShortTag() != "!!null") {
		if p.Kind != yaml.ScalarNode || p.ShortTag() != "!!str" {
			ui.ErrorTo(stderr, "kubeone: addons.path in %s is not a string", manifest)
			return "", noop, ui.Handled(fmt.Errorf("kubeone: addons.path in %s: %s", manifest, p.ShortTag()))
		}
		path = p.Value
	}

	made, err := mkdirTemp(workDir, runPrefix)
	if err != nil {
		ui.ErrorTo(stderr, "kubeone: cannot make a run directory in %s", workDir)
		return "", noop, ui.Handled(fmt.Errorf("kubeone: run directory: %w", err))
	}
	run, err := filepath.Abs(made)
	if err != nil {
		_ = os.RemoveAll(made)
		return "", noop, fmt.Errorf("kubeone: run directory: %w", err)
	}
	wrote := false
	cleanup := func() {
		_ = os.RemoveAll(run)
		if wrote {
			_ = os.Remove(run + ".yaml")
		}
	}
	copyBytes := raw
	if path != "" {
		src := path
		if !filepath.IsAbs(src) {
			abs, err := filepath.Abs(workDir)
			if err != nil {
				cleanup()
				return "", noop, fmt.Errorf("kubeone: work dir: %w", err)
			}
			src = filepath.Join(abs, src)
		}
		if err := copyCheckedAddons(stderr, src, filepath.Clean(path), filepath.Join(run, "addons")); err != nil {
			cleanup()
			return "", noop, err
		}
		setKey(ensureMapPath(root, "addons"), "path", strNode(filepath.Join(run, "addons")))
		if copyBytes, err = encodeYAML(root); err != nil {
			cleanup()
			return "", noop, fmt.Errorf("kubeone: manifest copy: %w", err)
		}
	}
	if err := writeExclusive(run+".yaml", copyBytes); err != nil {
		// A file that was there already stays; writeExclusive removed a
		// copy that it made and could not write to its end.
		cleanup()
		ui.ErrorTo(stderr, "kubeone: cannot write the manifest copy %s.yaml", run)
		return "", noop, ui.Handled(fmt.Errorf("kubeone: manifest copy: %w", err))
	}
	wrote = true
	return filepath.Base(run) + ".yaml", cleanup, nil
}

// copyCheckedAddons copies the directory src into the new directory dst:
// each directory with mode 0700, and each file with the bytes that passed
// the keys-only apply guard, with mode 0600. The order is that of
// filepath.WalkDir (bash: the same, directory by directory, the names
// sorted by bytes), so both name the same first file that they refuse.
// what names a file in the guard's message: shown (the addons path of the
// manifest) joined with the path of the file in src. A file that is not a
// regular file (a FIFO, a device, a symbolic link to a directory) and a
// directory that cannot be read are refused as a file that cannot be
// read (readCheckedRegular). A src that does not exist or is not a
// directory gets no copy; a src that cannot be read (a stat error other
// than "does not exist") is refused.
func copyCheckedAddons(stderr io.Writer, src, shown, dst string) error {
	fi, err := os.Stat(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return build.RefuseUnreadable(stderr, "kubeone", shown)
	case !fi.IsDir():
		return nil
	}
	root, err := filepath.EvalSymlinks(src)
	if err != nil {
		return build.RefuseUnreadable(stderr, "kubeone", shown)
	}
	return filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		what, target := filepath.Join(shown, rel), filepath.Join(dst, rel)
		if err == nil && de.IsDir() {
			return os.Mkdir(target, 0o700)
		}
		// A file (a symbolic link too: lo reads what it points at), or a
		// directory that cannot be read: the guard refuses the latter.
		if err != nil {
			return build.RefuseUnreadable(stderr, "kubeone", what)
		}
		checked, cerr := readCheckedRegular(stderr, what, path)
		if cerr != nil {
			return cerr
		}
		if werr := writeExclusive(target, checked); werr != nil {
			ui.ErrorTo(stderr, "kubeone: cannot write the run copy of %s", what)
			return ui.Handled(fmt.Errorf("kubeone: run copy: %w", werr))
		}
		return nil
	})
}

// readCheckedRegular reads the regular file at path once and checks the
// bytes with the keys-only apply guard (build.ReadCheckedStream). It opens
// the file without blocking (O_NONBLOCK) and checks the open file, not the
// path: an entry that is not a regular file is refused as a file that
// cannot be read, also one that replaced the file after the walk listed
// it. The open does not wait for a writer of a FIFO, and lo does not read
// a FIFO or a device, as a read could wait or never end.
func readCheckedRegular(stderr io.Writer, what, path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, build.RefuseUnreadable(stderr, "kubeone", what)
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, build.RefuseUnreadable(stderr, "kubeone", what)
	}
	return build.ReadCheckedStream(stderr, "kubeone", what, f)
}

// countDocs returns the number of YAML documents in raw (yq counts the
// same: an empty document after a last --- is one too).
func countDocs(raw []byte) (int, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	n := 0
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		n++
	}
}

// explode returns a copy of the tree under n with each alias replaced by a
// copy of the node that it names, and each merge key (<<) of a mapping
// replaced by the keys that it merges (yq: explode(.) with
// --yaml-fix-merge-anchor-to-spec). A key of the mapping itself wins over
// a merged one; of the merged mappings, the first one wins. Anchors are
// dropped. budget is the number of nodes left: an alias bomb stops with an
// error.
func explode(n *yaml.Node, budget *int) (*yaml.Node, error) {
	if n == nil {
		return nil, nil
	}
	if *budget--; *budget < 0 {
		return nil, errors.New("the manifest expands to too many nodes")
	}
	if n.Kind == yaml.AliasNode {
		if n.Alias == nil {
			return nil, errors.New("an alias names no node")
		}
		return explode(n.Alias, budget)
	}
	out := *n
	out.Anchor, out.Alias, out.Content = "", nil, nil
	if n.Kind != yaml.MappingNode {
		for _, c := range n.Content {
			e, err := explode(c, budget)
			if err != nil {
				return nil, err
			}
			out.Content = append(out.Content, e)
		}
		return &out, nil
	}
	own := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i]; !isMergeKey(k) {
			own[k.Value] = true
		}
	}
	seen := map[string]bool{}
	add := func(k, v *yaml.Node) error {
		ek, err := explode(k, budget)
		if err != nil {
			return err
		}
		ev, err := explode(v, budget)
		if err != nil {
			return err
		}
		seen[k.Value] = true
		out.Content = append(out.Content, ek, ev)
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if !isMergeKey(k) {
			if err := add(k, v); err != nil {
				return nil, err
			}
			continue
		}
		merged, err := explode(v, budget)
		if err != nil {
			return nil, err
		}
		maps := []*yaml.Node{merged}
		if merged.Kind == yaml.SequenceNode {
			maps = merged.Content
		}
		for _, m := range maps {
			if m.Kind != yaml.MappingNode {
				return nil, errors.New("a merge key (<<) merges a node that is not a mapping")
			}
			for j := 0; j+1 < len(m.Content); j += 2 {
				mk := m.Content[j]
				if own[mk.Value] || seen[mk.Value] {
					continue
				}
				if err := add(mk, m.Content[j+1]); err != nil {
					return nil, err
				}
			}
		}
	}
	return &out, nil
}

// isMergeKey reports whether k is the merge key << (a plain scalar, which
// yaml.v3 tags !!merge; a quoted "<<" is a string).
func isMergeKey(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.ShortTag() == "!!merge"
}

// writeExclusive writes data to a new file with mode 0600: it fails when
// the path exists, whatever is there (a file, a FIFO, a symbolic link),
// and that stays. When it made the file but cannot write it to its end
// (a full disk, a failed close), it removes the file: a part of a copy
// can hold credentials.
func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = writeData(f, data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

// writeData writes the bytes of a new file of writeExclusive (a variable,
// so a test can make the write fail after the file exists).
var writeData = func(f *os.File, data []byte) (int, error) { return f.Write(data) }

// encodeYAML is the encoding of saveYAMLDoc (2-space indent) into bytes.
func encodeYAML(root *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
