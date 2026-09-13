// Package assets ships the framework's first-party files inside the binary
// and materializes them into a project on first use (the "eject model").
//
// What is embedded: a committed mirror of the WHOLE framework tree under
// internal/assets/lok8s/. Two halves live in it. The data half is what a
// cluster applies: every bootstrap addon (addons/**), the driver cluster
// templates (drivers/{lo,kubeone,capi}/cluster/**), the ClusterInventory
// CRD mirror (libs/inventory/manifests/), the lo chat defaults (chat/) and
// the Tilt extension (tilt/, what the project-root Tiltfile loads). The
// code half is the frozen bash implementation: the `lo` entrypoint,
// libs/**, utils/**, the drivers' main + libs, the provider plugins and
// VERSION (bashtree.go: what LO_IMPL=bash and the provider bridge run).
// The mirror is canonical; the repo's .lok8s/** twin (the parity harnesses
// read it) is kept byte-identical, executable bits included, by
// hack/sync-legacy-assets.sh and TestEmbeddedMirrorMatchesLegacyTree.
//
// Precedence: an on-disk `.lok8s/<rel>` in the project WINS over the
// embedded copy. lo never overwrites an existing local file — the only
// writer of an existing file is `lo assets update`.
//
// Materialization: Resolve ejects the unit an asset belongs to (one addon,
// one driver template tree, …) into `.lok8s/<unit>/` the first time a
// consumer needs it and no local copy exists, together with a `.lo-origin`
// marker (lo version, timestamp, per-file sha256) so a later lo can tell
// "local edit" from "lo shipped a new version" (`lo assets diff`). Peek
// never writes into the project; under PolicyNever (`--no-eject`,
// LO_ASSETS_EJECT=never) neither does Resolve — both then serve the
// embedded copy from a per-run temp dir.
package assets

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/fsutil"
)

//go:embed all:lok8s
var mirrorFS embed.FS

// FS is the embedded mirror rooted at "lok8s": the whole .lok8s tree (lo,
// addons/, chat/, drivers/, libs/, providers/, tilt/, utils/, VERSION).
func FS() fs.FS {
	sub, err := fs.Sub(mirrorFS, "lok8s")
	if err != nil {
		panic(err)
	}
	return sub
}

// MarkerFile is the per-unit origin marker written next to ejected files.
const MarkerFile = ".lo-origin"

// Origin says where a resolved path came from.
type Origin string

const (
	// OriginLocal — the project's own copy under .lok8s/<rel> (pre-existing
	// or ejected earlier). Precedence winner.
	OriginLocal Origin = "local"
	// OriginEjected — no local copy existed; this call wrote the embedded
	// unit into the project (with its marker) and the path points there.
	OriginEjected Origin = "ejected"
	// OriginEmbedded — served from the embedded copy in a per-run temp dir;
	// nothing was written into the project (Peek, or PolicyNever).
	OriginEmbedded Origin = "embedded"
	// OriginNone — neither a local nor an embedded copy exists; the path is
	// where a local copy WOULD live, so callers report it like bash did.
	OriginNone Origin = "none"
)

// Policy is the materialization policy for Resolve.
type Policy int

const (
	// PolicyEject writes the embedded unit into the project on first use
	// (the default).
	PolicyEject Policy = iota
	// PolicyNever serves the embedded copy from a temp dir and never writes
	// into the project (`--no-eject`, LO_ASSETS_EJECT=never).
	PolicyNever
)

// EnvEject is the environment opt-out: "never" selects PolicyNever.
const EnvEject = "LO_ASSETS_EJECT"

var (
	// ErrInvalidRel rejects a rel that escapes the tree or is empty.
	ErrInvalidRel = errors.New("assets: invalid asset path")
	// ErrNotAsset marks a rel that no embedded unit covers.
	ErrNotAsset = errors.New("assets: not an embedded asset")

	mu        sync.Mutex
	policy    = PolicyEject
	policySet bool
	tempRoot  string
	tempDone  = map[string]bool{}
	// Stderr receives the one-line eject notices; tests redirect it.
	Stderr io.Writer = os.Stderr
	// Now is the marker timestamp source (SOURCE_DATE_EPOCH-aware).
	Now = now
)

// Configure sets the policy for this process: noEject or LO_ASSETS_EJECT=never
// select PolicyNever, anything else PolicyEject. The cli calls it once from
// the root command's pre-run; tests call SetPolicy.
func Configure(noEject bool) {
	if noEject || strings.EqualFold(os.Getenv(EnvEject), "never") {
		SetPolicy(PolicyNever)
		return
	}
	SetPolicy(PolicyEject)
}

// SetPolicy overrides the policy (tests, or a caller that never wants
// writes).
func SetPolicy(p Policy) {
	mu.Lock()
	defer mu.Unlock()
	policy = p
	policySet = true
}

// CurrentPolicy reports the effective policy: an explicit SetPolicy/
// Configure wins, else the environment is consulted at call time (so a
// consumer used without the cli still honors LO_ASSETS_EJECT=never).
func CurrentPolicy() Policy {
	mu.Lock()
	defer mu.Unlock()
	if policySet {
		return policy
	}
	if strings.EqualFold(os.Getenv(EnvEject), "never") {
		return PolicyNever
	}
	return policy
}

// Cleanup removes the per-run temp dir (main defers it). The exec shim
// never reaches here and never uses this dir: the bash tree it execs lives
// in the versioned cache or its temp twin (bashtree.go), both reused
// across runs, so nothing is left behind by the exec.
func Cleanup() {
	mu.Lock()
	defer mu.Unlock()
	if tempRoot != "" {
		_ = os.RemoveAll(tempRoot)
		tempRoot = ""
		tempDone = map[string]bool{}
	}
}

// Unit is one materialization unit: the smallest tree lo ejects, diffs and
// updates as a whole. Addons are one unit each; a driver's cluster
// templates, the inventory CRD mirror, the chat defaults and the Tilt
// extension are one unit per tree.
type Unit struct {
	// Rel is the unit's path below .lok8s/ ("addons/cilium",
	// "drivers/lo/cluster", …).
	Rel string
	// Kind classifies the unit.
	Kind UnitKind
}

// UnitKind is the class of an asset unit, as `lo assets list` prints it.
type UnitKind string

const (
	// KindAddon is a bootstrap addon under addons/<name>.
	KindAddon UnitKind = "addon"
	// KindDriver is a driver's cluster template tree.
	KindDriver UnitKind = "driver"
	// KindInventory is the ClusterInventory CRD mirror.
	KindInventory UnitKind = "inventory"
	// KindChat is the lo chat defaults.
	KindChat UnitKind = "chat"
	// KindTilt is the Tilt extension the project-root Tiltfile loads.
	KindTilt UnitKind = "tilt"
	// KindBash is the code half of the tree: the frozen bash implementation
	// (lo, libs/**, utils/**, the drivers' main + libs, providers/**,
	// VERSION). One unit, addressed by the reserved rel BashRel; its local
	// dir is .lok8s itself and its marker .lok8s/.lo-origin.
	KindBash UnitKind = "bash"
)

// BashRel is the rel that names the bash unit (`lo assets eject bash`). It
// is a reserved word, not a path: the unit's files live directly below
// .lok8s, beside the data units.
const BashRel = "bash"

// bashUnit is the one KindBash unit.
var bashUnit = Unit{Rel: BashRel, Kind: KindBash}

// dataUnits lists every unit but the bash one (the units that own a
// subtree of their own).
func dataUnits() []Unit {
	var out []Unit
	for _, u := range Units() {
		if u.Kind != KindBash {
			out = append(out, u)
		}
	}
	return out
}

// treeUnits are the non-addon units, in display order.
var treeUnits = []Unit{
	{Rel: "drivers/lo/cluster", Kind: KindDriver},
	{Rel: "drivers/kubeone/cluster", Kind: KindDriver},
	{Rel: "drivers/capi/cluster", Kind: KindDriver},
	{Rel: "libs/inventory/manifests", Kind: KindInventory},
	{Rel: "chat", Kind: KindChat},
	{Rel: "tilt", Kind: KindTilt},
}

var (
	unitsOnce sync.Once
	unitList  []Unit
)

// Units lists every embedded unit: the addons (bytewise by name, like the
// bash `*/` glob), the driver/inventory/chat/tilt trees, then the bash
// unit.
func Units() []Unit {
	unitsOnce.Do(func() {
		entries, _ := fs.ReadDir(FS(), "addons")
		var names []string
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				names = append(names, e.Name())
			}
		}
		sort.Slice(names, func(i, j int) bool { return names[i]+"/" < names[j]+"/" })
		for _, n := range names {
			unitList = append(unitList, Unit{Rel: "addons/" + n, Kind: KindAddon})
		}
		unitList = append(unitList, treeUnits...)
		unitList = append(unitList, bashUnit)
	})
	return append([]Unit(nil), unitList...)
}

// AddonNames lists the embedded addon names in glob order.
func AddonNames() []string {
	var names []string
	for _, u := range Units() {
		if u.Kind == KindAddon {
			names = append(names, strings.TrimPrefix(u.Rel, "addons/"))
		}
	}
	return names
}

// UnitFor returns the embedded unit that covers rel (rel itself or a path
// below it). The data units are matched first; the bash unit covers
// BashRel and every embedded path that no data unit owns or contains
// ("lo", "libs/build", "utils", … but not "drivers", which holds a data
// unit). ok is false when no unit covers it — e.g. an addon name the
// binary does not ship.
func UnitFor(rel string) (Unit, bool) {
	rel = path.Clean(rel)
	for _, u := range dataUnits() {
		if rel == u.Rel || strings.HasPrefix(rel, u.Rel+"/") {
			return u, true
		}
	}
	if rel == BashRel || bashCovers(rel) {
		return bashUnit, true
	}
	return Unit{}, false
}

// bashCovers reports whether rel is an embedded path that belongs to the
// bash unit: it exists in the mirror and no data unit lies at or below it.
func bashCovers(rel string) bool {
	if rel == "." || rel == "" {
		return false
	}
	if _, err := fs.Stat(FS(), rel); err != nil {
		return false
	}
	for _, u := range dataUnits() {
		if u.Rel == rel || strings.HasPrefix(u.Rel, rel+"/") {
			return false
		}
	}
	return true
}

// dataUnitOwns reports whether the embedded path fp lies inside a data
// unit (the bash unit's walk skips those subtrees).
func dataUnitOwns(fp string) bool {
	for _, u := range dataUnits() {
		if fp == u.Rel || strings.HasPrefix(fp, u.Rel+"/") {
			return true
		}
	}
	return false
}

// fsRoot is where the unit's files start in the embedded mirror.
func (u Unit) fsRoot() string {
	if u.Kind == KindBash {
		return "."
	}
	return u.Rel
}

// unitRel maps an embedded path inside the unit to its unit-relative
// slash path (the marker key).
func (u Unit) unitRel(fp string) string {
	if u.Kind == KindBash {
		return fp
	}
	return strings.TrimPrefix(fp, u.Rel+"/")
}

// unitDir is the unit's local root: .lok8s/<rel> for a data unit, .lok8s
// itself for the bash unit.
func unitDir(p *config.Paths, u Unit) string {
	if u.Kind == KindBash {
		return p.Lok8s
	}
	return localPath(p, u.Rel)
}

// unitExists is the precedence test: a data unit's dir exists; the bash
// unit's entrypoint (.lok8s/lo) exists. The entrypoint is the key on
// purpose: a bash eject writes it LAST, so a half-written tree never
// counts as local (ejectBash). A cache tree (isCacheTree) is never local:
// nothing in it counts, nothing is written into it.
func unitExists(p *config.Paths, u Unit) bool {
	if isCacheTree(p.Lok8s) {
		return false
	}
	if u.Kind == KindBash {
		return fsutil.FileExists(filepath.Join(p.Lok8s, "lo"))
	}
	return fsutil.DirExists(localPath(p, u.Rel))
}

// cleanRel validates and normalizes a rel: slash-separated, relative, no
// "..", not empty. Every consumer builds rel from already-validated names,
// so this is the belt to their braces.
func cleanRel(rel string) (string, error) {
	rel = filepath.ToSlash(rel)
	if rel == "" || strings.HasPrefix(rel, "/") {
		return "", ErrInvalidRel
	}
	c := path.Clean(rel)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", ErrInvalidRel
	}
	if slices.Contains(strings.Split(c, "/"), "..") {
		return "", ErrInvalidRel
	}
	return c, nil
}

// localPath is where rel lives (or would live) in the project.
func localPath(p *config.Paths, rel string) string {
	return filepath.Join(p.Lok8s, filepath.FromSlash(rel))
}

// LocalExists reports whether the project holds its own copy of the unit
// covering rel (the precedence test: the unit dir exists on disk).
func LocalExists(p *config.Paths, rel string) bool {
	rel, err := cleanRel(rel)
	if err != nil {
		return false
	}
	u, ok := UnitFor(rel)
	if !ok {
		_, err := os.Stat(localPath(p, rel))
		return err == nil
	}
	return unitExists(p, u)
}

// Resolve returns the on-disk path for rel ("addons/cilium",
// "drivers/lo/cluster/registry", "chat/defaults.json", …): the project's
// own copy when it exists, otherwise the embedded copy — ejected into the
// project under PolicyEject, served from a temp dir under PolicyNever. A rel
// that is neither local nor embedded resolves to its would-be local path
// with OriginNone (no error), so callers keep reporting "not found" the
// way the bash implementation did.
func Resolve(p *config.Paths, rel string) (string, Origin, error) {
	return resolve(p, rel, CurrentPolicy())
}

// Peek is Resolve without side effects on the project: local when present,
// else the embedded copy from the temp dir. Read-only commands (lint,
// audit, listings) use it.
func Peek(p *config.Paths, rel string) (string, Origin, error) {
	return resolve(p, rel, PolicyNever)
}

func resolve(p *config.Paths, rel string, pol Policy) (string, Origin, error) {
	rel, err := cleanRel(rel)
	if err != nil {
		return "", OriginNone, err
	}
	local := localPath(p, rel)
	u, embedded := UnitFor(rel)
	if !embedded {
		if _, err := os.Stat(local); err == nil {
			return local, OriginLocal, nil
		}
		return local, OriginNone, nil
	}
	if rel == BashRel {
		local = unitDir(p, u)
	}
	if unitExists(p, u) {
		// Precedence: the project's copy wins, whatever its content.
		return local, OriginLocal, nil
	}
	if isCacheTree(p.Lok8s) {
		pol = PolicyNever // never write into the binary's cache
	}
	if pol == PolicyNever {
		root, err := tempUnit(u)
		if err != nil {
			return "", OriginNone, err
		}
		if u.Kind == KindBash {
			if rel == BashRel {
				return root, OriginEmbedded, nil
			}
			return filepath.Join(root, filepath.FromSlash(rel)), OriginEmbedded, nil
		}
		return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rel, u.Rel))), OriginEmbedded, nil
	}
	if err := eject(p, u); err != nil {
		return "", OriginNone, err
	}
	return local, OriginEjected, nil
}

// tempUnit materializes unit u under the per-run temp dir (once) and
// returns the unit's root there.
func tempUnit(u Unit) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	if tempRoot == "" {
		dir, err := os.MkdirTemp("", "lo-assets-")
		if err != nil {
			return "", err
		}
		tempRoot = dir
	}
	root := tempRoot
	if u.Kind != KindBash {
		root = filepath.Join(tempRoot, filepath.FromSlash(u.Rel))
	}
	if tempDone[u.Rel] {
		return root, nil
	}
	if err := writeUnit(u, root); err != nil {
		return "", err
	}
	tempDone[u.Rel] = true
	return root, nil
}

// Eject writes the embedded unit covering rel into the project. It is the
// explicit form of the first-use eject (`lo assets eject`); it refuses to
// touch an existing local copy (ErrExists) — precedence, never overwrite.
func Eject(p *config.Paths, rel string) (Unit, error) {
	rel, err := cleanRel(rel)
	if err != nil {
		return Unit{}, err
	}
	u, ok := UnitFor(rel)
	if !ok {
		return Unit{}, fmt.Errorf("%w: %s", ErrNotAsset, rel)
	}
	if unitExists(p, u) {
		return u, fmt.Errorf("%w: %s", ErrExists, unitDir(p, u))
	}
	if isCacheTree(p.Lok8s) {
		return u, fmt.Errorf("%w: %s", ErrCacheTree, p.Lok8s)
	}
	return u, eject(p, u)
}

// ErrCacheTree marks a write aimed at the binary's cache tree (PATH_LOK8S
// naming it).
var ErrCacheTree = errors.New("assets: PATH_LOK8S names the bash tree cache, not a project tree")

// isCacheTree reports whether dir is a tree the binary extracted (it
// carries the cache manifest).
func isCacheTree(dir string) bool {
	return dir != "" && fsutil.FileExists(filepath.Join(dir, config.CacheMarker))
}

// ErrExists marks an explicit eject onto an existing local copy.
var ErrExists = errors.New("assets: local copy exists")

// eject writes unit u into the project atomically: the files land in a
// sibling temp dir that is renamed into place, so a crash never leaves a
// half-written unit that the next run would honor as "local". The bash
// unit shares .lok8s with the data units and takes its own path
// (ejectBash).
func eject(p *config.Paths, u Unit) error {
	if u.Kind == KindBash {
		return ejectBash(p)
	}
	dest := localPath(p, u.Rel)
	if _, err := os.Stat(dest); err == nil {
		return nil // raced with ourselves; precedence holds
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dest)+".lo-eject-")
	if err != nil {
		return err
	}
	if err := writeUnit(u, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	m, err := markerFor(u)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := m.write(filepath.Join(tmp, MarkerFile)); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	won, err := renameInto(tmp, dest)
	if err != nil {
		return err
	}
	if !won {
		return nil // another lo ejected the same unit first; precedence holds
	}
	fmt.Fprintf(Stderr, "[assets] ejected %s -> %s (review with: lo assets diff %s)\n", u.Rel, config.RelTo(p.Base, dest), u.Rel)
	return nil
}

// renameInto moves the staged unit tmp to dest. Two lo processes ejecting
// the same unit at once both stage a copy and race on the rename; the
// loser's rename fails with EEXIST/ENOTEMPTY (dest is a populated directory
// by then). That is not an error — the unit is on disk, byte-identical, and
// precedence says never overwrite — so the loser drops its stage and
// reports won=false. Any other failure is returned as is; tmp is removed on
// every path but success.
func renameInto(tmp, dest string) (won bool, err error) {
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.RemoveAll(tmp)
		// syscall.Errno maps both EEXIST and ENOTEMPTY onto fs.ErrExist.
		if errors.Is(err, fs.ErrExist) {
			if info, statErr := os.Stat(dest); statErr == nil && info.IsDir() {
				return false, nil
			}
		}
		return false, err
	}
	return true, nil
}

// walkUnit visits every embedded file of unit u: fp is the path in the
// mirror, rel the unit-relative key. The bash unit's walk skips the data
// units' subtrees.
func walkUnit(u Unit, visit func(fp, rel string, data []byte) error) error {
	return fs.WalkDir(FS(), u.fsRoot(), func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if u.Kind == KindBash && fp != "." && dataUnitOwns(fp) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(FS(), fp)
		if err != nil {
			return err
		}
		return visit(fp, u.unitRel(fp), data)
	})
}

// writeUnit copies the embedded unit's files below root, executable bits
// restored (go:embed drops them; fileMode holds the list).
func writeUnit(u Unit, root string) error {
	return walkUnit(u, func(fp, rel string, data []byte) error {
		return writeEmbedded(fp, filepath.Join(root, filepath.FromSlash(rel)), data)
	})
}

// writeEmbedded writes one embedded file (fp names it in the mirror) to
// target, creating the parent and applying the file's mode.
func writeEmbedded(fp, target string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, data, fileMode(fp)) // #nosec G306 -- framework files, 0644 or 0755 by list
}

// EmbeddedFiles lists the unit's files (slash paths relative to the unit,
// sorted) with their sha256.
func EmbeddedFiles(u Unit) (map[string]string, error) {
	out := map[string]string{}
	err := walkUnit(u, func(_, rel string, data []byte) error {
		out[rel] = hashBytes(data)
		return nil
	})
	return out, err
}

// LocalFiles lists the files under dir (slash paths relative to dir,
// marker excluded) with their sha256. A missing dir is an empty map.
func LocalFiles(dir string) (map[string]string, error) {
	return localFiles(dir, nil)
}

// localUnitFiles is LocalFiles over the unit's local dir; for the bash
// unit the data units' subtrees (their own units) are left out.
func localUnitFiles(p *config.Paths, u Unit) (map[string]string, error) {
	if u.Kind != KindBash {
		return LocalFiles(unitDir(p, u))
	}
	return localFiles(p.Lok8s, dataUnitOwns)
}

func localFiles(dir string, skip func(rel string) bool) (map[string]string, error) {
	out := map[string]string{}
	if _, err := os.Stat(dir); err != nil {
		return out, nil
	}
	// A project may link its tree (the parity harnesses do); WalkDir does
	// not descend a symlinked root, so resolve it first.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	err := filepath.WalkDir(dir, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, fp)
		rel = filepath.ToSlash(rel)
		if skip != nil && rel != "." && skip(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || path.Base(rel) == MarkerFile {
			return nil
		}
		data, err := os.ReadFile(fp)
		if err != nil {
			return err
		}
		out[rel] = hashBytes(data)
		return nil
	})
	return out, err
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Marker is the .lo-origin file: what lo ejected, when, and the hash of
// every file it wrote. `lo assets diff` reads it as the ORIGIN side.
type Marker struct {
	// Lo is the lo version that ejected the unit.
	Lo string `yaml:"lo"`
	// EjectedAt is the RFC-3339 UTC timestamp.
	EjectedAt string `yaml:"ejectedAt"`
	// Files maps the unit-relative slash path to "sha256:<hex>".
	Files map[string]string `yaml:"files"`
}

func markerFor(u Unit) (*Marker, error) {
	files, err := EmbeddedFiles(u)
	if err != nil {
		return nil, err
	}
	return &Marker{Lo: Version(), EjectedAt: Now(), Files: files}, nil
}

// write serializes the marker as a small, stable YAML document with a
// comment header (keys sorted; hashes are plain scalars, a path that needs
// quoting is double-quoted). The hand-written form keeps the bytes stable
// across lo versions; ReadMarker parses it with the YAML library.
func (m *Marker) write(file string) error {
	var b strings.Builder
	b.WriteString("# .lo-origin — written by lo when it ejected this asset. Do not edit.\n")
	b.WriteString("# `lo assets diff` compares ORIGIN (these hashes) vs LOCAL vs the copy\n")
	b.WriteString("# embedded in the running lo; `lo assets update` refreshes it.\n")
	fmt.Fprintf(&b, "lo: %s\n", yamlScalar(m.Lo))
	fmt.Fprintf(&b, "ejectedAt: %s\n", yamlScalar(m.EjectedAt))
	b.WriteString("files:\n")
	keys := make([]string, 0, len(m.Files))
	for k := range m.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %s\n", yamlScalar(k), m.Files[k])
	}
	return os.WriteFile(file, []byte(b.String()), 0o644)
}

// yamlScalar double-quotes anything that is not a plain, safe scalar.
func yamlScalar(s string) string {
	safe := s != "" && !strings.ContainsAny(s, ":#{}[],&*?|<>=!%@`\"'\\\n\t") && !strings.HasPrefix(s, " ") && !strings.HasSuffix(s, " ") && s != "-"
	if safe {
		return s
	}
	return strconv.Quote(s)
}

// ReadMarker parses a .lo-origin file. A missing file is (nil, nil).
func ReadMarker(file string) (*Marker, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	m := &Marker{}
	if err := yaml.Unmarshal(raw, m); err != nil {
		return nil, fmt.Errorf("assets: malformed marker %s: %w", file, err)
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	return m, nil
}

// now is the marker timestamp (bash inventory::_now's contract: RFC-3339
// UTC, SOURCE_DATE_EPOCH honored for reproducible runs).
func now() string {
	if v := os.Getenv("SOURCE_DATE_EPOCH"); v != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			n = 0
		}
		return time.Unix(n, 0).UTC().Format(time.RFC3339)
	}
	return time.Now().UTC().Format(time.RFC3339)
}
