package cli

// cmd_doctor_suc.go — the kubectl range check of `lo doctor` for the
// system-upgrade-controller addon (bash: doctor::_suc_check in
// .lok8s/libs/doctor; the piped lines are the same in both).
//
// The drain container of a Plan with cordon or drain runs the kubectl image
// that the addon pins, and that image serves only the Kubernetes minors of
// addons.SUCKubectlRange (internal/addons/suc.go has the details). The check
// reads the spec only: no kubectl, no cluster.
//
// It runs when spec.bootstrap has system-upgrade-controller as a bare entry
// name (read with the entry parser of `lo addons --detail`, as the RBAC
// migration check reads its addons). spec.kubernetes.version comes from the
// one reader (lodriver.KubernetesVersion, bash: spec::kubernetes_version).
// The lines: ✓ when the minor is in the range; ! with the range and the
// fixes when it is not; ℹ for a Lo cluster without a version (kind runs its
// default node image); ! (not checked) when the version cannot be read or is
// not vX.Y or vX.Y.Z. A cluster of another kind without a version gets no
// line: lo knows no default version for its driver, and no lint rule or
// schema requires the field there. Without a domain, with a deploy
// domain, without a cluster spec or with a spec whose kind cannot be read,
// it prints nothing: the RBAC migration check above says it. Every line is
// advisory.

import (
	"io"
	"path/filepath"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	lodriver "github.com/kernpilot/lok8s/internal/driver/lo"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

// The fixed parts of the lines.
const (
	sucNotChecked        = "system-upgrade-controller kubectl range not checked: "
	sucReasonUnreadable  = "spec.kubernetes.version cannot be read"
	sucReasonNotAVersion = "spec.kubernetes.version is not a version of the form vX.Y or vX.Y.Z"
)

// sucInRangeLine is the line for a minor in the range.
func sucInRangeLine(minor string) string {
	return "system-upgrade-controller: the kubectl image of the drain container serves Kubernetes " + minor +
		" (it serves " + addons.SUCKubectlRange() + ")"
}

// sucUnsetLine is the line for a Lo cluster without a version.
func sucUnsetLine() string {
	return "system-upgrade-controller: spec.kubernetes.version is not set, so kind runs its default node image (" +
		toolchain.KindDefaultNodeVersion + " with kind " + toolchain.KindVersion + ", the pinned release)" +
		"; the kubectl image of the drain container serves Kubernetes " + addons.SUCKubectlRange()
}

// doctorSUCKubectl prints the line of the check under the domain section.
// resolve reads spec.bootstrap (bootstrapEntries in production).
func doctorSUCKubectl(paths *config.Paths, d string, resolve addons.EntryResolver, out io.Writer) {
	if !domain.NameRe.MatchString(d) {
		return
	}
	spec := filepath.Join(paths.Clusters, d, "cluster.lok8s.yaml")
	if !fsutil.FileExists(spec) {
		return
	}
	kind, err := domain.SpecDriver(spec, "lo")
	if err != nil {
		return
	}
	// The addon counts only as a bare entry name (Builtin), never as a path
	// entry, as in the RBAC migration check.
	enabled := false
	for _, e := range resolve(spec, kind, d) {
		if e.Builtin && filepath.Base(e.Dir) == addons.SUCAddon {
			enabled = true
		}
	}
	if !enabled {
		return
	}
	version, err := lodriver.KubernetesVersion(spec)
	switch {
	case err != nil:
		doctorWarn(out, sucNotChecked+sucReasonUnreadable)
		return
	case version == "":
		if kind == "lo" {
			doctorInfo(out, sucUnsetLine())
		}
		return
	}
	switch state, minor := addons.SUCKubectl(version); state {
	case addons.SUCKubectlInRange:
		doctorOK(out, sucInRangeLine(minor))
	case addons.SUCKubectlOutOfRange:
		doctorWarn(out, addons.SUCKubectlOutOfRangeLine(minor))
	default:
		doctorWarn(out, sucNotChecked+sucReasonNotAVersion)
	}
}
