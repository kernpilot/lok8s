package lo

// config.go — Lo driver config readers, validators, and spec-env export
// (.lok8s/drivers/lo/utils/config.sh).
//
// Exported env interface (set after readConfig — the same names the bash
// exported; Tilt's local(), the bootstrap addon renders and the build
// envsubst whitelist read them):
//
//	KIND_EXPERIMENTAL_DOCKER_NETWORK  — docker bridge name
//	LOK8S_NETWORK_CIDR                — project /24 subnet
//	LOK8S_NETWORK_SUBNET              — alias for CIDR
//	LOK8S_NETWORK_BASE_IP             — /24 base (e.g. 10.125.130.0)
//	LOK8S_REGISTRY_IP_BUILD / _CACHE / _IO_DOCKER / … (mirrors upcased, - → _)
//	LOK8S_REGISTRY_SHARED / _NETWORK / _NETWORK_CIDR / _TLS / _PORT / _JSON
//	LOK8S_CP_COUNT, LOK8S_WORKER_COUNT, LOK8S_HOST_PORTS
//	LOK8S_EXTRA_MOUNTS_COUNT, LOK8S_MAX_CONCURRENT_DOWNLOADS
//	LOK8S_LB_POOL                     — MetalLB IP range
//	LOK8S_REMOTE_MODE, LOK8S_REMOTE_EXPOSE, LOK8S_REMOTE_SYNC_*, LOK8S_REMOTE_TILT

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/kernpilot/lok8s/internal/oidc"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ── Validation ────────────────────────────────────────────

// validateIPs counts EVERY bad IP before aborting (bash: lo::validate_ips —
// a validator that stops at the first error hides the rest of a broken
// layout). Reads the registry set from the live .registries.json so callers
// (and tests) that mutate the file between the read and the validate are
// observed, exactly like the per-call jq in bash.
func validateIPs(subnet, metallbPool string, errOut io.Writer) error {
	errors := 0

	subnetIP := strings.SplitN(subnet, "/", 2)[0]
	if !ipValidateFormat(subnetIP, errOut) {
		errors++
	}

	// Validate registry IPs from JSON.
	if rf, err := regFile(); err == nil {
		sharedCIDR := rf.Network.CIDR
		for _, r := range rf.Registries {
			targetSubnet := subnet
			if rf.Shared && r.Type == "mirror" {
				targetSubnet = sharedCIDR
			}
			if !ipValidateFormat(r.IP, errOut) {
				errors++
				continue
			}
			if !ipInSubnet(r.IP, targetSubnet) {
				fmt.Fprintf(errOut, "error: registry '%s' IP %s is outside subnet %s\n", r.Name, r.IP, targetSubnet)
				errors++
			}
		}
	}

	if metallbPool != "" {
		// bash: start = ${pool%-*} (strip after LAST -), end = ${pool#*-}
		// (strip through FIRST -).
		poolStart := metallbPool
		if i := strings.LastIndex(metallbPool, "-"); i >= 0 {
			poolStart = metallbPool[:i]
		}
		poolEnd := metallbPool
		if i := strings.Index(metallbPool, "-"); i >= 0 {
			poolEnd = metallbPool[i+1:]
		}
		if !ipValidateFormat(poolStart, errOut) {
			errors++
		} else if !ipInSubnet(poolStart, subnet) {
			fmt.Fprintf(errOut, "error: MetalLB pool start %s is outside subnet %s\n", poolStart, subnet)
			errors++
		}
		if !ipValidateFormat(poolEnd, errOut) {
			errors++
		} else if !ipInSubnet(poolEnd, subnet) {
			fmt.Fprintf(errOut, "error: MetalLB pool end %s is outside subnet %s\n", poolEnd, subnet)
			errors++
		}
		startInt, okS := ipToInt(poolStart)
		endInt, okE := ipToInt(poolEnd)
		if okS && okE && startInt > endInt {
			fmt.Fprintf(errOut, "error: MetalLB pool start %s is greater than end %s\n", poolStart, poolEnd)
			errors++
		}
	}

	if errors > 0 {
		fmt.Fprintf(errOut, "error: %d IP validation error(s). Aborting.\n", errors)
		return fmt.Errorf("%d IP validation error(s)", errors)
	}
	return nil
}

// ── Slot helper ───────────────────────────────────────────

var slotDomainRe = regexp.MustCompile(`^([0-9]+)\.lok8s\.dev$`)

// slotFromDomain derives a numeric slot from spec.cluster.domain (bash:
// lo::slot_from_domain). "" for non-*.lok8s.dev domains and out-of-range
// slots (valid: 2..199; 200 is the shared-registry net, 125 the default).
func slotFromDomain(clusterYAML string) string {
	root := loadYAML(clusterYAML)
	domain := yqOr(root, "", "spec", "cluster", "domain")
	if domain == "" {
		return ""
	}
	if domain == DefaultDomain {
		return strconv.Itoa(DefaultSlot)
	}
	if m := slotDomainRe.FindStringSubmatch(domain); m != nil {
		slot, _ := strconv.Atoi(m[1])
		if slot >= 2 && slot <= 199 {
			return m[1]
		}
	}
	return ""
}

// ── Config readers ────────────────────────────────────────

// readNetworkConfig resolves spec.network (with the *.lok8s.dev slot
// defaults) and regenerates .registries.json (bash:
// lo::read_network_config). Error strings are the raw `error: …` family,
// verbatim.
func readNetworkConfig(clusterYAML string, errOut io.Writer) error {
	// Missing spec is its own error, checked BEFORE any read: under `set -e`
	// a failing yq on a missing file would kill the caller before the
	// intended message could ever print.
	if !fileExists(clusterYAML) {
		fmt.Fprintf(errOut, "error: cluster spec not found: %s\n", clusterYAML)
		return fmt.Errorf("cluster spec not found: %s", clusterYAML)
	}

	root := loadYAML(clusterYAML)
	netName := yqOr(root, "", "spec", "network", "name")
	netCIDR := yqOr(root, "", "spec", "network", "cidr")

	if netName == "" || netCIDR == "" {
		if slot := slotFromDomain(clusterYAML); slot != "" {
			if netName == "" {
				netName = yqOr(root, "", "metadata", "name")
			}
			if netCIDR == "" {
				netCIDR = "10.125." + slot + ".0/24"
			}
		}
	}

	// Error with exactly what was inspected: the FILE and which fallback
	// branch was even eligible — a message claiming "metadata.name was empty"
	// on a path that never read it once sent a whole investigation to the
	// wrong file.
	if netName == "" {
		fmt.Fprintf(errOut, "error: %s: spec.network.name is missing (slot defaults only apply to *.lok8s.dev domains). Is this a Lo (kind) cluster spec?\n", clusterYAML)
		return fmt.Errorf("spec.network.name missing in %s", clusterYAML)
	}
	if netCIDR == "" {
		fmt.Fprintf(errOut, "error: %s: spec.network.cidr is required (e.g. \"10.125.50.0/24\" for slot 50; defaults only apply to *.lok8s.dev domains)\n", clusterYAML)
		return fmt.Errorf("spec.network.cidr missing in %s", clusterYAML)
	}

	baseIP := strings.SplitN(netCIDR, "/", 2)[0]

	os.Setenv("KIND_EXPERIMENTAL_DOCKER_NETWORK", netName)
	os.Setenv("LOK8S_NETWORK_CIDR", netCIDR)
	os.Setenv("LOK8S_NETWORK_SUBNET", netCIDR)
	os.Setenv("LOK8S_NETWORK_BASE_IP", baseIP)

	_, err := configGenerate(clusterYAML, errOut)
	return err
}

// readNodeConfig resolves spec.nodes (bash: lo::read_node_config).
func readNodeConfig(clusterYAML string, errOut io.Writer) error {
	defaultHostPorts := "false"
	if slotFromDomain(clusterYAML) == strconv.Itoa(DefaultSlot) {
		defaultHostPorts = "true"
	}

	root := loadYAML(clusterYAML)

	cpCount, workerCount, hostPorts := "1", "0", defaultHostPorts
	if yqPresent(root, "spec", "nodes") {
		cpCount = yqOr(root, "1", "spec", "nodes", "controlPlane")
		workerCount = yqOr(root, "0", "spec", "nodes", "workers")
		hp := yqRaw(root, "spec", "nodes", "hostPorts")
		if hp != "null" && hp != "" {
			hostPorts = hp
		}
	}

	extraMounts := len(yqSeq(root, "spec", "nodes", "extraMounts"))

	maxDownloads := yqRaw(root, "spec", "nodes", "maxConcurrentDownloads")
	if maxDownloads == "null" || maxDownloads == "" {
		maxDownloads = "3"
	} else if !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(maxDownloads) {
		fmt.Fprintf(errOut, "error: spec.nodes.maxConcurrentDownloads must be a positive integer, got '%s'\n", maxDownloads)
		return fmt.Errorf("invalid spec.nodes.maxConcurrentDownloads: %s", maxDownloads)
	}

	os.Setenv("LOK8S_CP_COUNT", cpCount)
	os.Setenv("LOK8S_WORKER_COUNT", workerCount)
	os.Setenv("LOK8S_HOST_PORTS", hostPorts)
	os.Setenv("LOK8S_EXTRA_MOUNTS_COUNT", strconv.Itoa(extraMounts))
	os.Setenv("LOK8S_MAX_CONCURRENT_DOWNLOADS", maxDownloads)
	return nil
}

// readLBConfig resolves spec.loadBalancer.pool with the *.lok8s.dev slot
// default (bash: lo::read_lb_config).
func readLBConfig(clusterYAML string) {
	root := loadYAML(clusterYAML)

	pool := ""
	if yqPresent(root, "spec", "loadBalancer") {
		pool = yqOr(root, "", "spec", "loadBalancer", "pool")
	}
	if pool == "" {
		if slot := slotFromDomain(clusterYAML); slot != "" {
			pool = "10.125." + slot + ".125-10.125." + slot + ".150"
		}
	}
	os.Setenv("LOK8S_LB_POOL", pool)
}

var syncDestRe = regexp.MustCompile(`^[A-Za-z0-9_./+-]+$`)

// readRemoteConfig resolves spec.remote (bash: lo::read_remote_config).
func readRemoteConfig(clusterYAML string, deps remoteDeps, errOut io.Writer) error {
	root := loadYAML(clusterYAML)

	mode := yqOr(root, "docker", "spec", "remote", "mode")

	expose := yqRaw(root, "spec", "remote", "expose")
	if expose == "null" || expose == "" {
		if deps.providerName != "" {
			expose = "true"
		} else {
			expose = "false"
		}
	}

	syncPath := yqOr(root, ".", "spec", "remote", "sync", "path")
	syncDest := yqOr(root, "/workspace", "spec", "remote", "sync", "dest")
	// Boundary validation: dest is interpolated into single-quoted REMOTE
	// shell commands (remote.go ssh mkdir/cd) — a quote or metacharacter in
	// it would break out of the quoting on the remote host. Plain path
	// charset only; no ~ either, since single quotes suppress tilde
	// expansion on the remote (a '~/x' dest would be taken literally and
	// fail at runtime).
	if !syncDestRe.MatchString(syncDest) {
		ui.Errorf(errOut, "spec.remote.sync.dest must be a plain absolute/relative path ([A-Za-z0-9_./+-], no ~), got: %s", syncDest)
		return fmt.Errorf("invalid spec.remote.sync.dest: %s", syncDest)
	}

	exclude := []string{".git", "node_modules", ".secrets", ".kubeconfig", "clusters/.active"}
	if yqPresent(root, "spec", "remote", "sync", "exclude") {
		exclude = nil
		for _, e := range yqSeq(root, "spec", "remote", "sync", "exclude") {
			if e.Value != "" {
				exclude = append(exclude, e.Value)
			}
		}
	}

	tilt := yqOr(root, "true", "spec", "remote", "tilt")

	os.Setenv("LOK8S_REMOTE_MODE", mode)
	os.Setenv("LOK8S_REMOTE_EXPOSE", expose)
	os.Setenv("LOK8S_REMOTE_SYNC_PATH", syncPath)
	os.Setenv("LOK8S_REMOTE_SYNC_DEST", syncDest)
	os.Setenv("LOK8S_REMOTE_TILT", tilt)
	// The exclude LIST cannot ride a scalar env var; the bash kept it as a
	// shell array. Newline-joined private env for the remote_ci consumer.
	os.Setenv("LOK8S_REMOTE_SYNC_EXCLUDE", strings.Join(exclude, "\n"))
	return nil
}

// remoteDeps carries the provider identity into readRemoteConfig (bash read
// the PROVIDER_NAME global).
type remoteDeps struct{ providerName string }

// readConfig reads all config sections in the correct order (bash:
// lo::read_config).
func readConfig(clusterYAML string, errOut io.Writer) error {
	if err := readNetworkConfig(clusterYAML, errOut); err != nil { // also generates the registry map
		return err
	}
	if err := readNodeConfig(clusterYAML, errOut); err != nil {
		return err
	}
	readLBConfig(clusterYAML)
	return nil
}

// ── Spec env export ───────────────────────────────────────

// exportSpecEnvs exports the LOK8S_SPEC_* env consumed by spec.bootstrap
// addon renders (bash: lo::export_spec_envs).
func exportSpecEnvs(clusterYAML string, errOut io.Writer) error {
	root := loadYAML(clusterYAML)

	os.Setenv("LOK8S_SPEC_CLUSTER_NAME", yqOr(root, "", "metadata", "name"))
	os.Setenv("LOK8S_SPEC_CLUSTER_DOMAIN", yqOr(root, "", "spec", "cluster", "domain"))
	os.Setenv("LOK8S_SPEC_CLUSTER_NAMESPACE", yqOr(root, "default", "spec", "cluster", "namespace"))
	os.Setenv("LOK8S_SPEC_KUBERNETES_VERSION", yqOr(root, "", "spec", "kubernetes", "version"))
	os.Setenv("LOK8S_SPEC_DNS_DOMAINFILTER", yqOr(root, "", "spec", "dns", "domainFilter"))

	// spec.oidc — apiserver StructuredAuthenticationConfiguration inputs
	// (consumed by renderAuthConfig and the kind render). Absent spec.oidc ⇒
	// ISSUER/CLIENTID empty ⇒ oidc.Enabled() false ⇒ NO apiserver OIDC
	// wiring (strict back-compat: the rendered kind config is unchanged).
	// Single source of truth for the reads + defaults: internal/oidc.
	if err := oidc.LoadSpec(clusterYAML, errOut); err != nil {
		return err
	}

	os.Setenv("LOK8S_SPEC_NETWORK_NAME", getenv("KIND_EXPERIMENTAL_DOCKER_NETWORK"))
	os.Setenv("LOK8S_SPEC_NETWORK_SUBNET", getenv("LOK8S_NETWORK_SUBNET"))
	os.Setenv("LOK8S_SPEC_NETWORK_BASE_IP", getenv("LOK8S_NETWORK_BASE_IP"))

	os.Setenv("LOK8S_SPEC_REGISTRY_PREFIX", yqOr(root, "lok8s.local", "spec", "registries", "prefix"))
	buildHost, cacheHost := "", ""
	if rf, err := regFile(); err == nil {
		buildHost = rf.get("build", "host")
		cacheHost = rf.get("cache", "host")
	}
	os.Setenv("LOK8S_SPEC_REGISTRY_BUILD_HOST", buildHost)
	os.Setenv("LOK8S_SPEC_REGISTRY_CACHE_HOST", cacheHost)
	os.Setenv("LOK8S_SPEC_REGISTRY_BUILD_IP", getenv("LOK8S_REGISTRY_IP_BUILD"))
	os.Setenv("LOK8S_SPEC_REGISTRY_CACHE_IP", getenv("LOK8S_REGISTRY_IP_CACHE"))

	pool := getenv("LOK8S_LB_POOL")
	os.Setenv("LOK8S_SPEC_LOADBALANCER_POOL", pool)
	if pool != "" && strings.Contains(pool, "-") {
		// Same %-* / #*- split as validateIPs.
		os.Setenv("LOK8S_SPEC_LOADBALANCER_POOL_START", pool[:strings.LastIndex(pool, "-")])
		os.Setenv("LOK8S_SPEC_LOADBALANCER_POOL_END", pool[strings.Index(pool, "-")+1:])
	} else {
		os.Setenv("LOK8S_SPEC_LOADBALANCER_POOL_START", "")
		os.Setenv("LOK8S_SPEC_LOADBALANCER_POOL_END", "")
	}

	os.Setenv("LOK8S_SPEC_KIND_PODSUBNET", DefaultPodCIDR)
	os.Setenv("LOK8S_SPEC_KIND_SERVICESUBNET", DefaultSvcCIDR)
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
