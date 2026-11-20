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
	// (go.mod: github.com/mgoltzsche/khelm/v2 v2.9.0) and the ChartRenderer
	// binary b installs under .kustomize/ for lo core.
	KhelmVersion = "2.9.0"
	// HelmVersion is helm.sh/helm/v3 as khelm v2.9.0 requires it — pinned in
	// go.mod as well, so the in-process chart inflation is the helm the khelm
	// binary was built from.
	HelmVersion = "3.22.1"
)

// YQVersion is the yq release the bash implementation is tested with: the
// contributor .bin/b.yaml and the consumer template pin it, and it is the
// minimum the bash messages and the docs name. An older release can stop on
// the bash version reader (v4.52.5 panics on its expressions). The drift
// tests check it against .bin/b.yaml, the template, the cloud-init fallback
// and every "yq v… or later" statement.
const YQVersion = "v4.54.3"

// The cluster tools. The kind release picks the default node image, which
// is the cluster's Kubernetes when spec.kubernetes.version is not set, and
// the node images that it supports: kind v0.32.0 defaults to kindest/node
// v1.36.1 and supports v1.33 to v1.36. kubectl must stay within one minor
// of that Kubernetes (the kubectl version skew policy).
//
// The b.yaml template pins KindVersion. KubectlVersion is the kubectl of the
// operator image only: the template's kubectl floats (see floatingEntries)
// until b installs kubectl from dl.k8s.io. A test in internal/assets ties
// the image's kind and kubectl downloads to these two: bump a pin, the image
// URL and its sums together.
const (
	KubectlVersion = "v1.36.2"
	KindVersion    = "v0.32.0"
)

// floatingEntries are the template entries that carry no version, each with
// the reason: b installs the latest release of each. Every other entry is
// pinned. A test fails on an entry that is neither pinned nor named here, so
// a new tool cannot float without a reason in this list.
var floatingEntries = map[string]string{
	"kubectl":                       "b's kubectl recipe downloads from a release bucket that stopped at v1.31.0, so a pin of a current release cannot install; pin it to KubectlVersion once b reads dl.k8s.io",
	"tilt":                          "the watcher and UI of the dev loop (local group); it renders no manifest",
	"mkcert":                        "makes the local CA for `lo trust` (local group); it renders no manifest",
	"github.com/kubermatic/kubeone": "an opt-in driver (cloud group); lok8s holds no KubeOne release to pin it to",
	"hcloud":                        "the Hetzner CLI of the opt-in cloud group; it renders no manifest",
	"github.com/arg-sh/argsh":       "the runtime of the opt-in bash group; its onPost installs the builtin of the same release",
	"jq":                            "a tool of the opt-in bash group (the frozen bash implementation)",
	"renvsubst":                     "a tool of the opt-in bash group (the frozen bash implementation)",
	"sops":                          "a tool of the opt-in bash group (the frozen bash implementation)",
	"ssh-to-age":                    "a tool of the opt-in bash group (the frozen bash implementation)",
}

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
	SecretPluginRel        = "secrets.lok8s.dev/v1/secret/Secret" // #nosec G101 -- a plugin path, not a credential
	ChartRendererPluginRel = "khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer"
)
