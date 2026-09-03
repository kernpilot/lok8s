// Package assets ships the framework's first-party data inside the binary
// and materializes it into a project on first use — the "eject model" of
// the Go migration's Phase 7 (WP1).
//
// What is embedded: a committed mirror of the framework tree under
// internal/assets/lok8s/ — every bootstrap addon (addons/**), the driver
// cluster templates (drivers/{lo,kubeone,capi}/cluster/**), the
// ClusterInventory CRD mirror (libs/inventory/manifests/), the lo chat
// defaults (chat/) and VERSION. The mirror is canonical; the repo's
// .lok8s/** twin (the frozen bash implementation and the parity harnesses
// read it) is kept byte-identical by hack/sync-legacy-assets.sh and
// TestEmbeddedMirrorMatchesLegacyTree.
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
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
)

//go:embed all:lok8s
var mirrorFS embed.FS

// FS is the embedded mirror rooted at "lok8s" (addons/, drivers/, libs/,
// chat/, VERSION).
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

// Cleanup removes the per-run temp dir (main defers it; the exec shim never
// reaches here, and never materialized anything before exec either).
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
// templates, the inventory CRD mirror and the chat defaults are one unit
// per tree.
type Unit struct {
	// Rel is the unit's path below .lok8s/ ("addons/cilium",
	// "drivers/lo/cluster", …).
	Rel string
	// Kind classifies the unit: addon | driver | inventory | chat.
	Kind string
}

// treeUnits are the non-addon units, in display order.
var treeUnits = []Unit{
	{Rel: "drivers/lo/cluster", Kind: "driver"},
	{Rel: "drivers/kubeone/cluster", Kind: "driver"},
	{Rel: "drivers/capi/cluster", Kind: "driver"},
	{Rel: "libs/inventory/manifests", Kind: "inventory"},
	{Rel: "chat", Kind: "chat"},
}

var (
	unitsOnce sync.Once
	unitList  []Unit
)

// Units lists every embedded unit: the addons (bytewise by name, like the
// bash `*/` glob) followed by the driver/inventory/chat trees.
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
			unitList = append(unitList, Unit{Rel: "addons/" + n, Kind: "addon"})
		}
		unitList = append(unitList, treeUnits...)
	})
	return append([]Unit(nil), unitList...)
}

// AddonNames lists the embedded addon names in glob order.
func AddonNames() []string {
	var names []string
	for _, u := range Units() {
		if u.Kind == "addon" {
			names = append(names, strings.TrimPrefix(u.Rel, "addons/"))
		}
	}
	return names
}

// UnitFor returns the embedded unit that covers rel (rel itself or a path
// below it). ok is false when no unit covers it — e.g. an addon name the
// binary does not ship.
func UnitFor(rel string) (Unit, bool) {
	rel = path.Clean(rel)
	for _, u := range Units() {
		if rel == u.Rel || strings.HasPrefix(rel, u.Rel+"/") {
			return u, true
		}
	}
	return Unit{}, false
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
	for _, seg := range strings.Split(c, "/") {
		if seg == ".." {
			return "", ErrInvalidRel
		}
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
	_, err = os.Stat(localPath(p, u.Rel))
	return err == nil
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
	if _, err := os.Stat(localPath(p, u.Rel)); err == nil {
		// Precedence: the project's copy wins, whatever its content.
		return local, OriginLocal, nil
	}
	if pol == PolicyNever {
		root, err := tempUnit(u)
		if err != nil {
			return "", OriginNone, err
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
	root := filepath.Join(tempRoot, filepath.FromSlash(u.Rel))
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
	if _, err := os.Stat(localPath(p, u.Rel)); err == nil {
		return u, fmt.Errorf("%w: %s", ErrExists, localPath(p, u.Rel))
	}
	return u, eject(p, u)
}

// ErrExists marks an explicit eject onto an existing local copy.
var ErrExists = errors.New("assets: local copy exists")

// eject writes unit u into the project atomically: the files land in a
// sibling temp dir that is renamed into place, so a crash never leaves a
// half-written unit that the next run would honor as "local".
func eject(p *config.Paths, u Unit) error {
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
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	fmt.Fprintf(Stderr, "[assets] ejected %s -> %s (review with: lo assets diff %s)\n", u.Rel, relToBase(p, dest), u.Rel)
	return nil
}

// relToBase prints dest relative to the project root when it lives there.
func relToBase(p *config.Paths, dest string) string {
	if rel, err := filepath.Rel(p.Base, dest); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return dest
}

// writeUnit copies the embedded unit's files below root (0644/0755).
func writeUnit(u Unit, root string) error {
	return fs.WalkDir(FS(), u.Rel, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(fp, u.Rel)
		target := filepath.Join(root, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(FS(), fp)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// EmbeddedFiles lists the unit's files (slash paths relative to the unit,
// sorted) with their sha256.
func EmbeddedFiles(u Unit) (map[string]string, error) {
	out := map[string]string{}
	err := fs.WalkDir(FS(), u.Rel, func(fp string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(FS(), fp)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(fp, u.Rel+"/")] = hashBytes(data)
		return nil
	})
	return out, err
}

// LocalFiles lists the files under dir (slash paths relative to dir,
// marker excluded) with their sha256. A missing dir is an empty map.
func LocalFiles(dir string) (map[string]string, error) {
	out := map[string]string{}
	if _, err := os.Stat(dir); err != nil {
		return out, nil
	}
	err := filepath.WalkDir(dir, func(fp string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, fp)
		rel = filepath.ToSlash(rel)
		if path.Base(rel) == MarkerFile {
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
	Lo string
	// EjectedAt is the RFC-3339 UTC timestamp.
	EjectedAt string
	// Files maps the unit-relative slash path to "sha256:<hex>".
	Files map[string]string
}

func markerFor(u Unit) (*Marker, error) {
	files, err := EmbeddedFiles(u)
	if err != nil {
		return nil, err
	}
	return &Marker{Lo: Version(), EjectedAt: Now(), Files: files}, nil
}

// write serializes the marker as a small, stable YAML document (keys
// sorted; no library needed, nothing to quote — hashes and paths are plain
// scalars, paths that need quoting are double-quoted).
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
	m := &Marker{Files: map[string]string{}}
	inFiles := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.HasPrefix(line, "  ") && inFiles {
			k, v, ok := splitMarkerEntry(strings.TrimSpace(line))
			if !ok {
				return nil, fmt.Errorf("assets: malformed marker line in %s: %q", file, line)
			}
			m.Files[k] = v
			continue
		}
		inFiles = false
		k, v, _ := strings.Cut(line, ":")
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "lo":
			m.Lo = unquote(v)
		case "ejectedAt":
			m.EjectedAt = unquote(v)
		case "files":
			inFiles = true
		}
	}
	return m, nil
}

// splitMarkerEntry parses `key: value` where key may be double-quoted (and
// then may itself contain ": ").
func splitMarkerEntry(line string) (string, string, bool) {
	if strings.HasPrefix(line, `"`) {
		q, err := strconv.QuotedPrefix(line)
		if err != nil {
			return "", "", false
		}
		k, err := strconv.Unquote(q)
		if err != nil {
			return "", "", false
		}
		rest := strings.TrimPrefix(line[len(q):], ":")
		return k, strings.TrimSpace(rest), strings.HasPrefix(line[len(q):], ":")
	}
	k, v, ok := strings.Cut(line, ": ")
	return k, strings.TrimSpace(v), ok
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
	}
	return s
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
