package assets

// diff.go — the three-way comparison behind `lo assets diff|show|list`,
// the origin column of `lo addons`/`lo drivers --list` and the doctor
// summary. Per file it compares ORIGIN (the .lo-origin marker: what lo
// ejected) against LOCAL (the project's copy) and EMBEDDED (what the
// running lo ships), by content hash.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
)

// FileState classifies one file of a unit.
type FileState string

const (
	// StateUnchanged — local == embedded (whatever origin says: converged).
	StateUnchanged FileState = "unchanged"
	// StateLocalModified — the project edited it (origin == embedded,
	// local differs); ALSO the verdict when no marker exists and local
	// differs: without an origin the local copy is authoritative and an
	// update needs --force.
	StateLocalModified FileState = "local modified"
	// StateLoUpdated — lo ships a newer copy, the project never touched
	// its own (origin == local, embedded differs). `lo assets update`
	// applies it.
	StateLoUpdated FileState = "lo updated"
	// StateBoth — a conflict: all three differ.
	StateBoth FileState = "both"
	// StateLocalOnly — exists in the project only.
	StateLocalOnly FileState = "local-only"
	// StateBuiltinOnly — exists in the embedded copy only (lo added it, or
	// the project deleted it).
	StateBuiltinOnly FileState = "builtin-only"
)

// Unit-level origin verdicts (the ORIGIN column).
const (
	OriginColBuiltin       = "builtin"
	OriginColLocal         = "local"
	OriginColLocalModified = "local (modified)"
	OriginColLocalOnly     = "local-only"
)

// FileDiff is one row of a unit's diff.
type FileDiff struct {
	Path     string    `json:"path"`
	State    FileState `json:"state"`
	Origin   string    `json:"origin,omitempty"`
	Local    string    `json:"local,omitempty"`
	Embedded string    `json:"embedded,omitempty"`
}

// MarkerInfo is the marker's header as reported.
type MarkerInfo struct {
	Lo        string `json:"lo"`
	EjectedAt string `json:"ejectedAt"`
}

// VersionPair is the chart-version headline of an addon unit ("-" when
// not a chart, or absent on that side).
type VersionPair struct {
	Local    string `json:"local"`
	Embedded string `json:"embedded"`
}

// UnitReport is the three-way verdict for one unit.
type UnitReport struct {
	Rel  string `json:"rel"`
	Kind string `json:"kind"`
	// Origin is the unit-level column: builtin · local · local (modified)
	// · local-only.
	Origin string `json:"origin"`
	// Drifted is true when any file is not unchanged (local-only files
	// excluded — a project may add files beside a unit's own).
	Drifted bool        `json:"drifted"`
	Version VersionPair `json:"version"`
	Marker  *MarkerInfo `json:"marker,omitempty"`
	Files   []FileDiff  `json:"files,omitempty"`
	// Path is the local unit dir (where it is, or would be).
	Path string `json:"path"`
}

// Counts summarizes a report's file states.
func (r UnitReport) Counts() map[FileState]int {
	c := map[FileState]int{}
	for _, f := range r.Files {
		c[f.State]++
	}
	return c
}

// Summary is the one-line status of a unit ("3 files: 1 lo updated, 1
// local modified, 1 unchanged").
func (r UnitReport) Summary() string {
	switch r.Origin {
	case OriginColBuiltin:
		return "not ejected"
	case OriginColLocalOnly:
		return fmt.Sprintf("%d local-only files", len(r.Files))
	}
	if !r.Drifted {
		return "in sync"
	}
	c := r.Counts()
	var parts []string
	for _, s := range []FileState{StateLoUpdated, StateLocalModified, StateBoth, StateBuiltinOnly, StateLocalOnly} {
		if c[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c[s], s))
		}
	}
	return strings.Join(parts, ", ")
}

// Report computes the three-way verdict for the given rels (every unit
// when rels is empty), embedded units first in Units() order, then any
// local-only addon dirs. Report never writes into the project.
func Report(p *config.Paths, rels []string) ([]UnitReport, error) {
	var units []Unit
	if len(rels) == 0 {
		units = Units()
		units = append(units, localOnlyAddons(p)...)
	} else {
		for _, rel := range rels {
			rel, err := cleanRel(rel)
			if err != nil {
				return nil, err
			}
			u, ok := UnitFor(rel)
			if !ok {
				if strings.HasPrefix(rel, "addons/") && dirExists(localPath(p, rel)) {
					u = Unit{Rel: rel, Kind: "addon"}
				} else {
					return nil, fmt.Errorf("%w: %s", ErrNotAsset, rel)
				}
			}
			units = append(units, u)
		}
	}
	var out []UnitReport
	for _, u := range units {
		r, err := reportUnit(p, u)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// localOnlyAddons finds addon dirs in the project that the binary does not
// ship (a project's own addons under .lok8s/addons/).
func localOnlyAddons(p *config.Paths) []Unit {
	entries, err := os.ReadDir(filepath.Join(p.Lok8s, "addons"))
	if err != nil {
		return nil
	}
	embedded := map[string]bool{}
	for _, n := range AddonNames() {
		embedded[n] = true
	}
	var units []Unit
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || embedded[e.Name()] {
			continue
		}
		if info, err := os.Stat(filepath.Join(p.Lok8s, "addons", e.Name())); err == nil && info.IsDir() {
			units = append(units, Unit{Rel: "addons/" + e.Name(), Kind: "addon"})
		}
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Rel+"/" < units[j].Rel+"/" })
	return units
}

func reportUnit(p *config.Paths, u Unit) (UnitReport, error) {
	dir := localPath(p, u.Rel)
	r := UnitReport{Rel: u.Rel, Kind: u.Kind, Path: dir, Version: VersionPair{Local: "-", Embedded: "-"}}
	_, isEmbedded := UnitFor(u.Rel)
	local, err := LocalFiles(dir)
	if err != nil {
		return r, err
	}
	var embedded map[string]string
	if isEmbedded {
		embedded, err = EmbeddedFiles(u)
		if err != nil {
			return r, err
		}
		if u.Kind == "addon" {
			r.Version.Embedded = chartVersionFS(u.Rel + "/chart.yaml")
		}
	}
	if !dirExists(dir) {
		r.Origin = OriginColBuiltin
		return r, nil
	}
	if u.Kind == "addon" {
		r.Version.Local = chartVersionFile(filepath.Join(dir, "chart.yaml"))
	}
	marker, err := ReadMarker(filepath.Join(dir, MarkerFile))
	if err != nil {
		return r, err
	}
	if marker != nil {
		r.Marker = &MarkerInfo{Lo: marker.Lo, EjectedAt: marker.EjectedAt}
	}
	if !isEmbedded {
		r.Origin = OriginColLocalOnly
		for _, f := range sortedKeys(local) {
			r.Files = append(r.Files, FileDiff{Path: f, State: StateLocalOnly, Local: local[f]})
		}
		return r, nil
	}
	r.Files = classify(marker, local, embedded)
	for _, f := range r.Files {
		if f.State != StateUnchanged && f.State != StateLocalOnly {
			r.Drifted = true
		}
	}
	if r.Drifted {
		r.Origin = OriginColLocalModified
	} else {
		r.Origin = OriginColLocal
	}
	return r, nil
}

// classify is the six-way matrix. origin may be nil (no marker).
func classify(marker *Marker, local, embedded map[string]string) []FileDiff {
	names := map[string]bool{}
	for k := range local {
		names[k] = true
	}
	for k := range embedded {
		names[k] = true
	}
	var out []FileDiff
	for _, f := range sortedKeys(names) {
		l, inL := local[f]
		e, inE := embedded[f]
		o := ""
		if marker != nil {
			o = marker.Files[f]
		}
		d := FileDiff{Path: f, Origin: o, Local: l, Embedded: e}
		switch {
		case inL && !inE:
			d.State = StateLocalOnly
		case !inL && inE:
			d.State = StateBuiltinOnly
		case l == e:
			d.State = StateUnchanged
		case o == "":
			d.State = StateLocalModified
		case o == l:
			d.State = StateLoUpdated
		case o == e:
			d.State = StateLocalModified
		default:
			d.State = StateBoth
		}
		out = append(out, d)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// chartVersionFS reads chart.yaml's version from the embedded mirror.
func chartVersionFS(rel string) string {
	raw, err := readEmbedded(rel)
	if err != nil {
		return "-"
	}
	return chartVersion(raw)
}

func chartVersionFile(file string) string {
	raw, err := os.ReadFile(file)
	if err != nil {
		return "-"
	}
	return chartVersion(raw)
}

// chartVersion mirrors addons.readChart's `yq -r '.version // "-"'`.
func chartVersion(raw []byte) string {
	var doc struct {
		Version any `yaml:"version"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil || doc.Version == nil {
		return "-"
	}
	return fmt.Sprint(doc.Version)
}

// ── rendering ────────────────────────────────────────────────────────────

// WriteTable prints the unit table (`lo assets list` / `lo assets diff`
// without a rel): aligned columns, one line per unit.
func WriteTable(w io.Writer, reports []UnitReport, withFiles bool) {
	fmt.Fprintf(w, "%-30s  %-10s  %-18s  %-10s  %-10s  %s\n", "ASSET", "KIND", "ORIGIN", "LOCAL", "EMBEDDED", "STATUS")
	fmt.Fprintf(w, "%-30s  %-10s  %-18s  %-10s  %-10s  %s\n", "-----", "----", "------", "-----", "--------", "------")
	for _, r := range reports {
		fmt.Fprintf(w, "%-30s  %-10s  %-18s  %-10s  %-10s  %s\n", r.Rel, r.Kind, r.Origin, r.Version.Local, r.Version.Embedded, r.Summary())
		if withFiles && len(r.Files) > 0 {
			WriteFiles(w, r, "  ")
		}
	}
}

// WriteFiles prints a unit's per-file rows.
func WriteFiles(w io.Writer, r UnitReport, indent string) {
	fmt.Fprintf(w, "%s%-40s  %s\n", indent, "FILE", "STATE")
	for _, f := range r.Files {
		fmt.Fprintf(w, "%s%-40s  %s\n", indent, f.Path, f.State)
	}
}

// WriteShow prints one unit in detail (`lo assets show <rel>`).
func WriteShow(w io.Writer, r UnitReport) {
	fmt.Fprintf(w, "asset:    %s\n", r.Rel)
	fmt.Fprintf(w, "kind:     %s\n", r.Kind)
	fmt.Fprintf(w, "origin:   %s\n", r.Origin)
	fmt.Fprintf(w, "path:     %s\n", r.Path)
	if r.Kind == "addon" {
		fmt.Fprintf(w, "version:  local %s, embedded %s\n", r.Version.Local, r.Version.Embedded)
	}
	if r.Marker != nil {
		fmt.Fprintf(w, "ejected:  by lo %s at %s\n", r.Marker.Lo, r.Marker.EjectedAt)
	} else if r.Origin != OriginColBuiltin {
		fmt.Fprintln(w, "ejected:  no .lo-origin marker (vendored copy; local edits cannot be told from lo updates)")
	}
	fmt.Fprintf(w, "status:   %s\n", r.Summary())
	if len(r.Files) > 0 {
		fmt.Fprintln(w)
		WriteFiles(w, r, "  ")
	}
}

// AnyDrift reports whether any unit drifted (`--check`).
func AnyDrift(reports []UnitReport) bool {
	for _, r := range reports {
		if r.Drifted {
			return true
		}
	}
	return false
}

// DoctorLine is the one-line summary `lo doctor` prints.
func DoctorLine(p *config.Paths) (line string, warn bool) {
	reports, err := Report(p, nil)
	if err != nil {
		return "assets: " + err.Error(), true
	}
	local, drifted := 0, 0
	for _, r := range reports {
		if r.Origin == OriginColBuiltin || r.Origin == OriginColLocalOnly {
			continue
		}
		local++
		if r.Drifted {
			drifted++
		}
	}
	switch {
	case local == 0:
		return "assets: none ejected (every asset served from the binary)", false
	case drifted == 0:
		return fmt.Sprintf("assets: %d local, all in sync with the binary", local), false
	default:
		return fmt.Sprintf("assets: %d of %d local assets drifted (lo assets diff)", drifted, local), true
	}
}
