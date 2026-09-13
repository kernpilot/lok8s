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
//     extracted once from the embed (stage dir + rename; the manifest
//     config.CacheMarker with one sha256 per file is verified on every
//     use, so a partial, stale or foreign-file extract is redone). A
//     relative XDG_CACHE_HOME is ignored, as the XDG spec says.
//  4. The same tree under os.TempDir()/lok8s-<uid>/<version>/lok8s when
//     no cache location can be derived (neither XDG_CACHE_HOME nor HOME)
//     or the cache cannot be written (read-only HOME). Same manifest,
//     reused across runs, so an exec'd shim leaks nothing.
//
// A checkout or ejected tree ALWAYS wins over the cache and is never
// written to. A cache tree is never local: PATH_LOK8S naming one (the
// shim exports it to bash children, and a nested Go lo inherits it) is
// classified TreeCache, and config.ResolvePaths ignores it for the
// project's .lok8s. The cache is outside the project, so the eject policy
// (--no-eject / LO_ASSETS_EJECT=never, "write nothing into the project")
// does not switch it off.
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
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// TreeSource says where a resolved bash tree comes from. It is typed so
// a caller can decide by source: a routing of commands to bash (WP8)
// accepts a project or checkout tree and refuses the cache.
type TreeSource string

const (
	// TreeProject — the project's own .lok8s holds lo (an ejected tree, or
	// a vendored one).
	TreeProject TreeSource = "project"
	// TreeCheckout — PATH_LOK8S points at another tree that holds lo (a
	// lok8s checkout).
	TreeCheckout TreeSource = "checkout"
	// TreeCache — the copy embedded in the binary, extracted into the
	// versioned cache.
	TreeCache TreeSource = "cache"
	// TreeTemp — the embedded copy under os.TempDir()/lok8s-<uid>/<version>
	// (no cache dir could be derived or written); verified and reused like
	// the cache.
	TreeTemp TreeSource = "temp"
	// TreeNone — nothing on disk yet (FindBashTree only): Dir is the cache
	// dir BashTree would fill, or empty when none can be derived.
	TreeNone TreeSource = "none"
)

// Local reports whether the source is a tree the project owns or points
// at (project or checkout): the only sources a routing may target.
func (s TreeSource) Local() bool { return s == TreeProject || s == TreeCheckout }

// Tree is a resolved bash tree: where it is and where it came from.
type Tree struct {
	Dir    string
	Source TreeSource
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
// extracting anything: TreeProject or TreeCheckout for a local tree,
// TreeCache for a cache that is already extracted and valid, else
// TreeNone with the cache dir BashTree would fill (empty when no cache
// location can be derived). `lo doctor` reads it.
func FindBashTree(p *config.Paths) Tree {
	if t, ok := localBashTree(p); ok {
		return t
	}
	dir, err := cacheTreeDir()
	if err != nil {
		return Tree{Source: TreeNone}
	}
	if cacheValid(dir) {
		return Tree{Dir: dir, Source: TreeCache}
	}
	return Tree{Dir: dir, Source: TreeNone}
}

// BashTree resolves the bash tree (precedence in the file comment),
// extracting the embedded copy into the cache on first use. A cache that
// cannot be derived or written falls back to the temp twin.
func BashTree(p *config.Paths) (Tree, error) {
	if t, ok := localBashTree(p); ok {
		return t, nil
	}
	dir, err := cacheTreeDir()
	if err == nil {
		if err = ensureCache(dir); err == nil {
			return Tree{Dir: dir, Source: TreeCache}, nil
		}
	}
	ui.DebugTo(Stderr, "assets: bash tree cache unavailable (%v); using the temp tree", err)
	tmp, err := tempCacheDir()
	if err != nil {
		return Tree{}, err
	}
	if err := ensureCache(tmp); err != nil {
		return Tree{}, err
	}
	return Tree{Dir: tmp, Source: TreeTemp}, nil
}

// localBashTree is precedence steps 1 and 2. p.Lok8s is the project's
// own .lok8s unless PATH_LOK8S moved it: the same dir is TreeProject,
// another one holding lo is TreeCheckout. A cache tree (the manifest at
// its root) is neither: it is skipped here and served as TreeCache by the
// caller, so nothing ever counts it as local.
func localBashTree(p *config.Paths) (Tree, bool) {
	project := filepath.Join(p.Base, ".lok8s")
	if !isCacheTree(p.Lok8s) && fsutil.FileExists(filepath.Join(p.Lok8s, "lo")) {
		if p.Lok8s == project {
			return Tree{Dir: p.Lok8s, Source: TreeProject}, true
		}
		return Tree{Dir: p.Lok8s, Source: TreeCheckout}, true
	}
	if project != p.Lok8s && !isCacheTree(project) && fsutil.FileExists(filepath.Join(project, "lo")) {
		return Tree{Dir: project, Source: TreeProject}, true
	}
	return Tree{}, false
}

// EnvCacheHome is the XDG variable that relocates the cache.
const EnvCacheHome = "XDG_CACHE_HOME"

// ErrNoCacheDir reports that no cache location can be derived.
var ErrNoCacheDir = errors.New("assets: no cache directory (set XDG_CACHE_HOME or HOME)")

// cacheTreeDir is ${XDG_CACHE_HOME:-$HOME/.cache}/lok8s/<version>/lok8s. A
// relative XDG_CACHE_HOME is invalid per the XDG base directory spec and
// is ignored (it would land the cache inside the working directory).
func cacheTreeDir() (string, error) {
	root := os.Getenv(EnvCacheHome)
	if !filepath.IsAbs(root) {
		root = ""
	}
	if root == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", ErrNoCacheDir
		}
		root = filepath.Join(home, ".cache")
	}
	return filepath.Join(root, "lok8s", Version(), "lok8s"), nil
}

// tempCacheDir is the cache's temp twin: os.TempDir()/lok8s-<uid>/<version>/lok8s.
// The per-user base dir is created 0700 and must stay a plain directory
// of this user with that mode (another user's or a symlink's tree would be
// code this process execs).
func tempCacheDir() (string, error) {
	base := filepath.Join(os.TempDir(), fmt.Sprintf("lok8s-%d", os.Getuid()))
	if err := os.Mkdir(base, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(base)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByUs(info) {
		return "", fmt.Errorf("assets: %s is not a private directory of this user (mode %o); remove it", base, info.Mode().Perm())
	}
	return filepath.Join(base, Version(), "lok8s"), nil
}

// cacheManifest is the whole-tree manifest the cache carries at its root
// (config.CacheMarker, the unit marker format; Files covers every file of
// the mirror). Lo pins the version the tree was extracted from.
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

// cacheValid reports whether dir holds EXACTLY the embedded tree: the
// manifest names this version and every file, every file on disk hashes
// to its entry with the right mode, and nothing else is there (no extra
// file, no symlink, no foreign entry). The tree is code this process
// execs, so a copy with anything unlisted is replaced, not trusted.
func cacheValid(dir string) bool {
	m, err := ReadMarker(filepath.Join(dir, config.CacheMarker))
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
		info, err := os.Lstat(target)
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
	// Nothing unlisted: every entry below dir is a listed file, its
	// directory, or the manifest.
	extra := false
	_ = filepath.WalkDir(dir, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			extra = true
			return fs.SkipAll
		}
		rel, _ := filepath.Rel(dir, fp)
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == config.CacheMarker {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if _, ok := want.Files[rel]; !ok || !d.Type().IsRegular() {
			extra = true
			return fs.SkipAll
		}
		return nil
	})
	return !extra
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
	if won {
		sweepCache(dir)
	}
	return nil
}

// staleStageAge is how old an abandoned stage dir must be before the sweep
// removes it; a younger one may belong to an extract still running.
const staleStageAge = time.Hour

// sweepCache removes what an earlier extract left beside dir: abandoned
// stage and aside dirs older than staleStageAge in the version dir, and
// the other <version>/ dirs under the cache root (only the running
// binary's version is kept; an older binary re-extracts on its next use).
func sweepCache(dir string) {
	parent := filepath.Dir(dir)
	prefix := "." + filepath.Base(dir) + ".lo-"
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > staleStageAge {
			_ = os.RemoveAll(filepath.Join(parent, e.Name()))
		}
	}
	root := filepath.Dir(parent)
	versions, _ := os.ReadDir(root)
	for _, v := range versions {
		if v.IsDir() && v.Name() != filepath.Base(parent) {
			_ = os.RemoveAll(filepath.Join(root, v.Name()))
		}
	}
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
	return m.write(filepath.Join(root, config.CacheMarker))
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
	switch t.Source {
	case TreeProject, TreeCheckout, TreeCache:
		return t.Dir + " (" + string(t.Source) + ")"
	case TreeTemp:
		return t.Dir + " (temp dir)"
	default:
		if t.Dir == "" {
			return "none (" + ErrNoCacheDir.Error() + ")"
		}
		return t.Dir + " (cache, not extracted yet)"
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
