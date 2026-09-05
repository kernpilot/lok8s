// Package toolchain owns the consumer toolchain of a lok8s project: the
// pinned versions of the tools `lo` execs, the .bin/b.yaml template that
// declares them for b (github.com/fentas/b), the verified bootstrap of b
// itself, and the doctor checks that verify what landed.
//
// The pins here are the SAME numbers the binary is built from — pins.go is
// drift-tested (pins_test.go) against go.mod and against the template, so
// bumping one without the others fails `go test`. That is what keeps the
// exec render (lo core: the pinned kustomize binary + the khelm and
// Secret exec plugins) byte-identical to the in-process render (lo-full:
// the same kustomize API release, khelm as a library).
package toolchain

// The render pins. Every number is a release that exists; bump them
// together and re-verify the byte parity (hack/parity-build.sh, the
// committed kubehz.dev render).
const (
	// KustomizeAPI is the sigs.k8s.io/kustomize/api module version go.mod
	// pins (lo-full renders through it).
	KustomizeAPI = "v0.21.1"
	// KustomizeCLI is the kustomize release built from KustomizeAPI — the
	// binary lo core execs and .bin/b.yaml pins. kustomizeAPIToCLI is the
	// mapping the drift test enforces.
	KustomizeCLI = "v5.8.1"
	// KhelmVersion is the khelm release: the library lo-full links
	// (go.mod: github.com/mgoltzsche/khelm/v2 v2.8.0) and the ChartRenderer
	// binary b installs under .kustomize/ for lo core.
	KhelmVersion = "2.8.0"
	// HelmVersion is helm.sh/helm/v3 as khelm v2.8.0 requires it — pinned in
	// go.mod as well, so the in-process chart inflation is the helm the khelm
	// binary was built from.
	HelmVersion = "3.21.2"
)

// kustomizeAPIToCLI maps the kustomize API module version to the CLI
// release it ships in (sigs.k8s.io/kustomize tags `kustomize/v5.x.y` next
// to `api/v0.x.y`; the pairs are read off the kustomize release notes).
// The drift test requires KustomizeCLI == kustomizeAPIToCLI[KustomizeAPI],
// so bumping go.mod's api version without extending this table fails.
var kustomizeAPIToCLI = map[string]string{
	"v0.21.1": "v5.8.1",
}

// Plugin paths under a project's .kustomize/ plugin home
// (<group>/<version>/<lowercase kind>/<Kind>), where kustomize resolves
// exec generators. The b.yaml template installs the binaries there via
// `file:` (relative to .bin/, hence the ../).
const (
	SecretPluginRel        = "secrets.lok8s.dev/v1/secret/Secret"
	ChartRendererPluginRel = "khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer"
)
