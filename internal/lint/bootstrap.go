package lint

// Bootstrap validation (bash: lint::bootstrap + the shared
// bootstrap::_resolve_entries / bootstrap::_parse_entry from
// .lok8s/libs/bootstrap). Entry resolution + parsing IS the apply path's
// (internal/bootstrapspec), so `lo lint` and `lo bootstrap` never disagree
// on what an entry means. Re-implementing the parse ad hoc is what used to
// break the map form: a plain `yq '.spec.bootstrap[]?'` shatters
// `- ccm: {wait: true, dependsOn: [...]}` into separate YAML lines, each then
// mis-read as a bogus addon name. A malformed entry (multi-key map, non-map
// value, bad name:) is reported by the parser itself and counted here.
//
// Each entry is carried alongside its compact-JSON rendering (bash: `yq
// -o=json -I=0`) because the JSON string IS part of the error messages.

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/bootstrapspec"
	"github.com/kernpilot/lok8s/internal/domain"
	lodriver "github.com/kernpilot/lok8s/internal/driver/lo"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// bootstrap validates each spec.bootstrap entry resolves to an existing addon
// directory (bash: lint::bootstrap). Returns the number of errors found.
func (l *Linter) bootstrap(domainDir, specFile, domainName string) int {
	// Only cluster specs carry spec.bootstrap
	if !fsutil.IsRegular(domainDir + "/cluster.lok8s.yaml") {
		return 0
	}
	kind, err := domain.SpecDriver(specFile, "")
	if err != nil {
		return 0
	}

	// The shared parser prints every validation failure through Report;
	// the merge hook only needs the merge's FAILURE mode (an unparseable
	// file), reported with the same message. (yq would additionally leak
	// its own parse error to stderr — an unexercised cosmetic difference.)
	parser := &bootstrapspec.Parser{
		Paths:  l.Paths,
		Report: func(format string, a ...any) { ui.ErrorTo(l.ErrOut, format, a...) },
		MergeValueFiles: func(files []string, _ *yaml.Node) error {
			for _, vf := range files {
				raw, err := os.ReadFile(vf)
				if err != nil {
					return err
				}
				if parseDocs(raw) == nil {
					return fmt.Errorf("lint: %s does not parse", vf)
				}
			}
			return nil
		},
	}

	errs := 0
	suc := false
	for _, item := range bootstrapspec.Resolve(firstDoc(specFile), kind) {
		e, ok := parser.Parse(domainName, item)
		if !ok {
			// The parser already emitted a specific "bootstrap: ..." error.
			errs++
			continue
		}
		if !fsutil.DirExists(e.Dir) {
			// Report the ORIGINAL entry (matches the apply path's
			// addon-not-found error) — the parsed name alone hides which YAML
			// entry failed for the path/name: forms.
			ui.ErrorTo(l.ErrOut, "  spec.bootstrap entry not found: %s (resolved to %s)", item.Raw, e.Dir)
			errs++
		}
		// The addon counts only as a bare entry name, as in `lo doctor`.
		if e.Builtin && filepath.Base(e.Dir) == addons.SUCAddon {
			suc = true
		}
	}
	if suc {
		l.sucKubectl(specFile)
	}
	return errs
}

// sucKubectl warns when spec.kubernetes.version is a minor that the kubectl
// image of system-upgrade-controller does not serve, with the line of `lo
// doctor` (internal/addons/suc.go; bash: lint::_suc_kubectl). A version that
// is not set, cannot be read or is not vX.Y or vX.Y.Z gets no line here;
// only `lo doctor` reports it. The version check of lint
// (Linter.kubernetesVersion) reads a Lo spec only, and it accepts every
// image tag, `latest` too. Warnings only.
func (l *Linter) sucKubectl(specFile string) {
	version, err := lodriver.KubernetesVersion(specFile)
	if err != nil || version == "" {
		return
	}
	if state, minor := addons.SUCKubectl(version); state == addons.SUCKubectlOutOfRange {
		ui.WarnTo(l.ErrOut, "  %s", addons.SUCKubectlOutOfRangeLine(minor))
	}
}
