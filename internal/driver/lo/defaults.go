// Package lo is the Go port of the Lo (kind) driver
// (.lok8s/drivers/lo/{main,utils/*.sh,libs/registry}): local/CI clusters via
// kind, with the project docker network, the framework registries
// (build/cache + pull-through mirrors), containerd certs.d config, CoreDNS
// wiring, the remote-VM CI mode, and the node-IP heal.
//
// Everything external (docker, kind, kubectl, ssh, rsync, scp, git, curl and
// the secrets kustomize plugin) runs through the injectable execx.Runner
// seam, so unit tests never touch a live daemon.
//
// State contract with the rest of the (partially still bash) pipeline: the
// bash driver communicated through exported environment variables
// (KIND_EXPERIMENTAL_DOCKER_NETWORK, LOK8S_NETWORK_*, LOK8S_REGISTRY_*,
// LOK8S_SPEC_*, LOK8S_REMOTE_*) and through the generated
// clusters/<domain>/.registries.json. Both are kept: the Go readers
// os.Setenv the exact same names (Tilt's local(), the bash bootstrap addons
// and the build envsubst whitelist all read them), and the JSON is generated
// byte-compatible with the bash jq output (consumers: lo main's header,
// main::down, libs/image, Tilt, tests).
package lo

// Constants from .lok8s/drivers/lo/utils/defaults.sh.
const (
	DefaultDomain  = "lok8s.dev"
	DefaultSlot    = 125
	DefaultPodCIDR = "10.244.0.0/16"
	DefaultSvcCIDR = "10.96.0.0/12"

	RegistryImage = "registry:2.8.3"
	// Listen ports. Plain-HTTP registries serve on :80; TLS registries serve
	// on :443 so that a bare-IP `docker push <ip>/...` (which the Docker
	// client resolves to the HTTPS default port 443) reaches them without an
	// explicit port in the ref. See REGISTRY-TLS.md for why the port is
	// TLS-mode-dependent.
	RegistryPort         = 80
	RegistryPortTLS      = 443
	RegistryOffsetBuild  = 101
	RegistryOffsetCache  = 102
	RegistryOffsetMirror = 103
	// In-container mount point for the registry cert+key (TLS mode).
	RegistryTLSMount = "/etc/registry/certs"

	SharedRegistryNetwork = "lok8s-registries"
	// Container-name prefix for SHARED mirrors — the ownership contract
	// between the recreate in registryNetwork (which may docker-rm matching
	// containers), containerFor (which names them), and RegistryClean. One
	// spelling; a rename that misses a site strands mirrors mid-recreate.
	SharedRegistryPrefix = "lok8s-registry-"
	SharedRegistryCIDR   = "10.125.200.0/24"
)

// registryStateDir resolves the durable home for rendered registry configs
// (bash: LO_REGISTRY_STATE_DIR). Each container bind-mounts
// <state-dir>/<container-name>.yaml — the file MUST outlive the `lo` run:
// with --restart=always the Docker daemon re-binds it on every restart, and
// a vanished source is recreated as an empty DIRECTORY, killing the registry
// (the old mktemp-and-delete approach broke every registry on reboot).
func registryStateDir() string {
	if v := getenv("LO_REGISTRY_STATE_DIR"); v != "" {
		return v
	}
	state := getenv("XDG_STATE_HOME")
	if state == "" {
		state = getenv("HOME") + "/.local/state"
	}
	return state + "/lok8s/registries"
}

// The documented spec defaults (docs/reference/specs.md, "Default
// resolution"): the ONE place the driver and `lo lint --notes` read them
// from, so what the driver fills in for an absent key and what lint
// reports as droppable change together.

// DefaultControlPlane is spec.nodes.controlPlane when absent.
const DefaultControlPlane = "1"

// DefaultRuntime is spec.runtime when absent.
const DefaultRuntime = "kind"

// Mirror is one spec.registries.mirrors entry: the registry name and its
// upstream URL.
type Mirror struct {
	Name, URL string
}

// DefaultMirrors is spec.registries.mirrors when absent or empty: the
// four standard upstreams. Returned as a fresh slice so a caller cannot
// change the default.
func DefaultMirrors() []Mirror {
	return []Mirror{
		{"io-docker", "https://registry-1.docker.io"},
		{"io-quay", "https://quay.io"},
		{"io-k8s", "https://registry.k8s.io"},
		{"io-ghcr", "https://ghcr.io"},
	}
}
