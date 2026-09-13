package assets

// bashtree.go — the frozen bash implementation as a runnable tree.
//
// The binary embeds the whole .lok8s tree (assets.go), so LO_IMPL=bash,
// the provider bridge, `lo recover` and a bash-only `lo drivers <name>`
// no longer need a checkout. BashTree hands them a directory that holds
// the complete tree, by precedence:
//
//  1. PATH_LOK8S when it is set and holds `lo` (a checkout, or a project
//     that ejected the bash unit) — config.Paths.Lok8s carries it.
//  2. <project>/.lok8s when it holds `lo` (the same, with PATH_LOK8S
//     pointing elsewhere).
//  3. A versioned cache, ${XDG_CACHE_HOME:-$HOME/.cache}/lok8s/<version>/lok8s,
//     extracted once from the embed (stage dir + rename; a manifest with
//     one sha256 per file is verified on every use, so a partial or stale
//     extract is redone).
//  4. A per-run temp dir when no cache location can be derived (neither
//     XDG_CACHE_HOME nor HOME is set).
//
// A checkout or ejected tree ALWAYS wins over the cache and is never
// written to. The cache is outside the project, so the eject policy
// (--no-eject / LO_ASSETS_EJECT=never, "write nothing into the project")
// does not switch it off; only the missing-HOME case falls back to the
// temp dir.
//
// The project's own .lok8s (config.Paths.Lok8s, where the data units are
// ejected) and the bash tree are the SAME directory only in a project that
// holds the bash unit (`lo assets eject bash`, which also ejects every
// data unit the project lacks so the tree is complete). Otherwise they
// differ: the data units live in the project, the bash tree in the cache,
// and the cache carries its own copy of the data half — bash mode then
// reads the binary's data files, not the project's ejected ones.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/fsutil"
)

// OriginCache — the tree was served from the versioned cache (BashTree).
const OriginCache Origin = "cache"

// Tree is a resolved bash tree: where it is and where it came from
// (OriginLocal, OriginCache or OriginEmbedded for the temp fallback;
// OriginNone from FindBashTree when nothing is on disk yet).
type Tree struct {
	Dir    string
	Origin Origin
}

// bashExecutables lists the mirror files that carry the executable bit
// (go:embed keeps content only). TestBashExecutablesMatchMirror pins it
// to the modes on disk; a script that gains or loses +x is added or
// removed here in the same change.
var bashExecutables = map[string]bool{
	"lo":                             true,
	"libs/addons":                    true,
	"libs/bootstrap":                 true,
	"libs/build":                     true,
	"libs/deploy":                    true,
	"libs/drivers":                   true,
	"libs/env":                       true,
	"libs/gitops":                    true,
	"libs/image":                     true,
	"libs/k8s":                       true,
	"libs/lint":                      true,
	"libs/provision":                 true,
	"libs/secrets":                   true,
	"libs/status":                    true,
	"libs/tilt":                      true,
	"providers/hetzner/cloud-config": true,
	"utils/provider.sh":              true,
}

// fileMode is the mode an embedded file is written with: 0755 for the
// listed scripts, 0644 otherwise.
func fileMode(fp string) fs.FileMode {
	if bashExecutables[fp] {
		return 0o755
	}
	return 0o644
}

// FindBashTree reports where BashTree would serve the tree from WITHOUT
// extracting anything: OriginLocal for a checkout or ejected tree,
// OriginCache for a cache that is already extracted and valid, else
// OriginNone with the cache dir BashTree would fill (empty when no cache
// location can be derived). `lo doctor` reads it.
func FindBashTree(p *config.Paths) Tree {
	if dir, ok := localBashTree(p); ok {
		return Tree{Dir: dir, Origin: OriginLocal}
	}
	dir, err := cacheTreeDir()
	if err != nil {
		return Tree{Origin: OriginNone}
	}
	if cacheValid(dir) {
		return Tree{Dir: dir, Origin: OriginCache}
	}
	return Tree{Dir: dir, Origin: OriginNone}
}

// BashTree resolves the bash tree (precedence in the file comment),
// extracting the embedded copy into the cache on first use.
func BashTree(p *config.Paths) (Tree, error) {
	if dir, ok := localBashTree(p); ok {
		return Tree{Dir: dir, Origin: OriginLocal}, nil
	}
	dir, err := cacheTreeDir()
	if err != nil {
		root, err := tempTree()
		if err != nil {
			return Tree{}, err
		}
		return Tree{Dir: root, Origin: OriginEmbedded}, nil
	}
	if err := ensureCache(dir); err != nil {
		return Tree{}, err
	}
	return Tree{Dir: dir, Origin: OriginCache}, nil
}

// localBashTree is precedence steps 1 and 2.
func localBashTree(p *config.Paths) (string, bool) {
	if fsutil.FileExists(filepath.Join(p.Lok8s, "lo")) {
		return p.Lok8s, true
	}
	project := filepath.Join(p.Base, ".lok8s")
	if project != p.Lok8s && fsutil.FileExists(filepath.Join(project, "lo")) {
		return project, true
	}
	return "", false
}

// EnvCacheHome is the XDG variable that relocates the cache.
const EnvCacheHome = "XDG_CACHE_HOME"

// ErrNoCacheDir reports that no cache location can be derived.
var ErrNoCacheDir = errors.New("assets: no cache directory (set XDG_CACHE_HOME or HOME)")

// cacheTreeDir is ${XDG_CACHE_HOME:-$HOME/.cache}/lok8s/<version>/lok8s.
func cacheTreeDir() (string, error) {
	root := os.Getenv(EnvCacheHome)
	if root == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", ErrNoCacheDir
		}
		root = filepath.Join(home, ".cache")
	}
	return filepath.Join(root, "lok8s", Version(), "lok8s"), nil
}

// cacheManifest is the whole-tree manifest the cache carries at its root
// (MarkerFile, the same format as a unit marker; Files covers every file
// of the mirror). Lo pins the version the tree was extracted from.
func cacheManifest() (*Marker, error) {
	files := map[string]string{}
	err := fs.WalkDir(FS(), ".", func(fp string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(FS(), fp)
		if err != nil {
			return err
		}
		files[fp] = hashBytes(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Marker{Lo: Version(), EjectedAt: Now(), Files: files}, nil
}

// cacheValid reports whether dir holds a complete, byte-identical copy of
// the embedded tree: the manifest names this version and every file, and
// every file on disk hashes to its entry with the right mode.
func cacheValid(dir string) bool {
	m, err := ReadMarker(filepath.Join(dir, MarkerFile))
	if err != nil || m == nil || m.Lo != Version() {
		return false
	}
	want, err := cacheManifest()
	if err != nil || len(m.Files) != len(want.Files) {
		return false
	}
	for fp, sum := range want.Files {
		if m.Files[fp] != sum {
			return false
		}
		target := filepath.Join(dir, filepath.FromSlash(fp))
		info, err := os.Stat(target)
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
		if (info.Mode()&0o100 != 0) != bashExecutables[fp] {
			return false
		}
		data, err := os.ReadFile(target)
		if err != nil || hashBytes(data) != sum {
			return false
		}
	}
	return true
}

// ensureCache extracts the embedded tree into dir unless a valid copy is
// there. The files are staged in a sibling dir and renamed into place; a
// stale or partial dir is moved aside first. Two processes extracting at
// once both stage; the loser's rename finds dir populated and re-verifies
// the winner's copy.
func ensureCache(dir string) error {
	if cacheValid(dir) {
		return nil
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+".lo-extract-")
	if err != nil {
		return err
	}
	if err := writeTree(stage); err != nil {
		_ = os.RemoveAll(stage)
		return err
	}
	if fsutil.Exists(dir) {
		// Move the stale copy aside under the stage's unique name, then
		// drop it; a concurrent extract that already replaced dir is fine.
		aside := stage + ".stale"
		if err := os.Rename(dir, aside); err != nil && !errors.Is(err, fs.ErrNotExist) {
			_ = os.RemoveAll(stage)
			return err
		}
		_ = os.RemoveAll(aside)
	}
	won, err := renameInto(stage, dir)
	if err != nil {
		return err
	}
	if !won && !cacheValid(dir) {
		return fmt.Errorf("assets: bash tree cache at %s is not the embedded tree (remove it and retry)", dir)
	}
	return nil
}

// writeTree writes the whole embedded tree below root with its manifest.
func writeTree(root string) error {
	err := fs.WalkDir(FS(), ".", func(fp string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(FS(), fp)
		if err != nil {
			return err
		}
		return writeEmbedded(fp, filepath.Join(root, filepath.FromSlash(fp)), data)
	})
	if err != nil {
		return err
	}
	m, err := cacheManifest()
	if err != nil {
		return err
	}
	return m.write(filepath.Join(root, MarkerFile))
}

// tempTree materializes the whole tree under the per-run temp dir (once)
// and returns it. Units already served from there stay where they are:
// the data units land at their own rels, so the temp root is one
// coherent tree.
func tempTree() (string, error) {
	for _, u := range Units() {
		if _, err := tempUnit(u); err != nil {
			return "", err
		}
	}
	mu.Lock()
	defer mu.Unlock()
	return tempRoot, nil
}

// ejectBash writes the bash unit (the code half) into the project's
// .lok8s, beside whatever data units are there. The files are staged in a
// temp dir under .lok8s and moved into place entry by entry (a directory
// that does not exist yet moves as a whole; one that does is merged); an
// existing file is never overwritten and counts as kept. The entrypoint
// `lo` moves LAST: it is the precedence key (unitExists), so a run that
// dies half-way leaves a tree the next eject completes instead of one
// that counts as local.
func ejectBash(p *config.Paths) error {
	dest := p.Lok8s
	if unitExists(p, bashUnit) {
		return nil // raced with ourselves; precedence holds
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(dest, ".bash.lo-eject-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := writeUnit(bashUnit, tmp); err != nil {
		return err
	}
	m, err := markerFor(bashUnit)
	if err != nil {
		return err
	}
	if err := m.write(filepath.Join(tmp, MarkerFile)); err != nil {
		return err
	}
	kept, err := mergeMove(tmp, dest, true)
	if err != nil {
		return err
	}
	note := ""
	if kept > 0 {
		note = fmt.Sprintf("; %d existing file(s) kept", kept)
	}
	fmt.Fprintf(Stderr, "[assets] ejected %s -> %s%s (review with: lo assets diff %s)\n", BashRel, config.RelTo(p.Base, dest), note, BashRel)
	return nil
}

// mergeMove moves every entry of src into dst: an entry absent from dst is
// renamed as a whole, a directory present on both sides is merged, a file
// present on both sides is kept (never overwritten) and counted. At the
// top level `lo` goes last.
func mergeMove(src, dst string, top bool) (kept int, err error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return 0, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if top {
		names = moveLast(names, "lo")
	}
	for _, name := range names {
		s, d := filepath.Join(src, name), filepath.Join(dst, name)
		if !fsutil.Exists(d) {
			if err := os.Rename(s, d); err == nil {
				continue
			} else if !errors.Is(err, fs.ErrExist) {
				return kept, err
			}
			// Raced: d appeared between the check and the rename.
		}
		if fsutil.DirExists(s) && fsutil.DirExists(d) {
			n, err := mergeMove(s, d, false)
			kept += n
			if err != nil {
				return kept, err
			}
			continue
		}
		kept++
	}
	return kept, nil
}

// moveLast returns names with name at the end, when present.
func moveLast(names []string, name string) []string {
	var out []string
	found := false
	for _, n := range names {
		if n == name {
			found = true
			continue
		}
		out = append(out, n)
	}
	if found {
		out = append(out, name)
	}
	return out
}

// String is the doctor spelling of a tree: its dir and where it comes
// from.
func (t Tree) String() string {
	switch t.Origin {
	case OriginLocal:
		return t.Dir + " (checkout or ejected tree)"
	case OriginCache:
		return t.Dir + " (cache)"
	case OriginEmbedded:
		return t.Dir + " (temp dir)"
	default:
		if t.Dir == "" {
			return "none (" + ErrNoCacheDir.Error() + ")"
		}
		return t.Dir + " (cache, extracted on first use)"
	}
}

// MissingDataUnits lists the rels of the data units the project does not
// hold yet. `lo assets eject bash` ejects them with the bash unit, so the
// project's .lok8s becomes a complete tree (what bash mode reads).
func MissingDataUnits(p *config.Paths) []string {
	var out []string
	for _, u := range dataUnits() {
		if !unitExists(p, u) {
			out = append(out, u.Rel)
		}
	}
	return out
}
