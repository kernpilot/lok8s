package lo

// configregistry.go — registry config: .registries.json generation + query
// helpers (.lok8s/drivers/lo/utils/config-registry.sh).
//
// JSON schema (BYTE-COMPATIBLE with the bash jq output — external consumers
// read the file: lo main's header, main::down, libs/image, Tilt, tests):
//
//	{
//	  "shared": true,
//	  "tls": false,
//	  "port": 80,
//	  "network": { "name": "lok8s-registries", "cidr": "10.125.200.0/24" },
//	  "project_network": "local",
//	  "registries": [
//	    { "name": "build", "ip": "…", "url": "", "domain": "", "host": "lok8s.local", "type": "framework" },
//	    …
//	  ]
//	}
//
// tls:  when true, registries serve HTTPS with a dev-CA-signed cert
//	(.secrets/tls/registries/) and clients trust them via the dev root CA —
//	no `insecure-registries` daemon config required.
// port: listen/connect port. 80 in plain-HTTP mode, 443 in TLS mode (so a
//	bare-IP `docker push` reaches the registry on the HTTPS default port).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Registry is one entry of .registries.json. Field order = JSON key order =
// the bash jq object order (byte-compat contract).
type Registry struct {
	Name   string `json:"name"`
	IP     string `json:"ip"`
	URL    string `json:"url"`
	Domain string `json:"domain"`
	Host   string `json:"host"`
	Type   string `json:"type"`
}

// RegistryNetworkInfo is the shared-network block of .registries.json.
type RegistryNetworkInfo struct {
	Name string `json:"name"`
	CIDR string `json:"cidr"`
}

// RegistryFile is the full .registries.json document.
type RegistryFile struct {
	Shared         bool                `json:"shared"`
	TLS            bool                `json:"tls"`
	Port           int                 `json:"port"`
	Network        RegistryNetworkInfo `json:"network"`
	ProjectNetwork string              `json:"project_network"`
	Registries     []Registry          `json:"registries"`
}

var mirrorNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// validateMirrorName mirrors lo::validate_mirror_name (raw error family).
func validateMirrorName(name string, errOut io.Writer) bool {
	if !mirrorNameRe.MatchString(name) {
		fmt.Fprintf(errOut, "error: invalid mirror name '%s': must match ^[a-z0-9][a-z0-9-]*$\n", name)
		return false
	}
	return true
}

// configGenerate generates clusters/<domain>/.registries.json for a cluster
// (bash: registry::config_generate). Requires the network config to have
// been read first (LOK8S_NETWORK_BASE_IP). Exports the same scalar env vars
// the bash did (LOK8S_REGISTRY_* — consumers: libs/image, Tilt, tests) and
// sets LOK8S_REGISTRY_JSON to the generated path.
func configGenerate(clusterYAML string, errOut io.Writer) (string, error) {
	domainDir := filepath.Dir(clusterYAML)

	projectSubnet := getenv("LOK8S_NETWORK_BASE_IP")
	if projectSubnet == "" {
		fmt.Fprintln(errOut, "error: lo::read_network_config must run before registry::config_generate")
		return "", fmt.Errorf("registry config: network config not read")
	}

	root := loadYAML(clusterYAML)

	// Read shared settings from spec. Default: NOT shared (flipped 2026-08-17,
	// was true). Shared mode dual-homes every kind node onto the registry
	// network, and docker's address resolution for a dual-homed container can
	// flip after endpoint churn — kind's entrypoint then pins kubelet's
	// --node-ip to the registry network, silently black-holing every route
	// INTO the node (observed live; see HealNodeIPs). Cross-project mirror
	// sharing is worth opting into, not a topology to impose by default.
	sharedEnabled := yqRaw(root, "spec", "registries", "shared", "enabled")
	if sharedEnabled == "null" || sharedEnabled == "" {
		sharedEnabled = "false"
	}

	// TLS mode (default true). When enabled, registries serve HTTPS on :443
	// with a cert minted by the Secret plugin (the `cert:` generator), signed
	// by the dev CA at CAROOT — no `insecure-registries` needed. Opt out with
	// `tls: false` (plain HTTP on :80, requires the registry IP range in
	// `insecure-registries`).
	tlsEnabled := yqRaw(root, "spec", "registries", "tls")
	switch tlsEnabled {
	case "false":
	case "true", "null", "":
		tlsEnabled = "true"
	default:
		fmt.Fprintf(errOut, "error: spec.registries.tls must be true or false, got '%s'\n", tlsEnabled)
		return "", fmt.Errorf("invalid spec.registries.tls: %s", tlsEnabled)
	}

	// Listen/connect port is TLS-mode-dependent (see defaults.go).
	regPort := RegistryPort
	if tlsEnabled == "true" {
		regPort = RegistryPortTLS
	}

	netName := yqOr(root, SharedRegistryNetwork, "spec", "registries", "shared", "network", "name")
	netCIDR := yqOr(root, SharedRegistryCIDR, "spec", "registries", "shared", "network", "cidr")

	sharedBase := strings.SplitN(netCIDR, "/", 2)[0]
	projectNetwork := envOr("KIND_EXPERIMENTAL_DOCKER_NETWORK", "lok8s")

	// Framework-private registries (always on project subnet).
	buildIP, _ := ipAdd(projectSubnet, RegistryOffsetBuild)
	cacheIP, _ := ipAdd(projectSubnet, RegistryOffsetCache)

	registries := []Registry{
		{Name: "build", IP: buildIP, Host: "lok8s.local", Type: "framework"},
		{Name: "cache", IP: cacheIP, Host: "lok8s.cache", Type: "framework"},
	}

	// Collect mirrors (user-defined or defaults).
	type mirror struct{ name, url string }
	var mirrors []mirror
	specMirrors := yqSeq(root, "spec", "registries", "mirrors")
	if len(specMirrors) == 0 {
		mirrors = []mirror{
			{"io-docker", "https://registry-1.docker.io"},
			{"io-quay", "https://quay.io"},
			{"io-k8s", "https://registry.k8s.io"},
			{"io-ghcr", "https://ghcr.io"},
		}
	} else {
		for i, m := range specMirrors {
			// yq -r prints the literal word "null" for a missing name — and
			// "null" PASSES the mirror-name regex, so the bash accepted it and
			// failed later on the url check (or created a mirror named
			// "null"). Kept as-is: bash wins over tidiness.
			name := yqRaw(m, "name")
			url := yqOr(m, "", "url")

			if !validateMirrorName(name, errOut) {
				return "", fmt.Errorf("invalid mirror name %q", name)
			}
			if name == "build" || name == "cache" {
				fmt.Fprintf(errOut, "error: spec.registries.mirrors: '%s' is reserved for the framework\n", name)
				return "", fmt.Errorf("reserved mirror name %q", name)
			}
			if url == "" {
				fmt.Fprintf(errOut, "error: spec.registries.mirrors[%d] (%s): url is required\n", i, name)
				return "", fmt.Errorf("mirror %q missing url", name)
			}
			mirrors = append(mirrors, mirror{name, url})
		}
	}

	// Allocate IPs and add mirror entries.
	for idx, m := range mirrors {
		var mirrorIP string
		if sharedEnabled == "true" {
			mirrorIP, _ = ipAdd(sharedBase, idx+2)
		} else {
			mirrorIP, _ = ipAdd(projectSubnet, RegistryOffsetCache+idx+1)
		}

		// Resolve the containerd-facing domain. For standard mirrors the
		// upstream domain differs from the registry API hostname
		// (e.g. docker.io pulls go to registry-1.docker.io).
		var domain string
		switch m.name {
		case "io-docker":
			domain = "docker.io"
		case "io-quay":
			domain = "quay.io"
		case "io-k8s":
			domain = "registry.k8s.io"
		case "io-ghcr":
			domain = "ghcr.io"
		default:
			domain = strings.TrimPrefix(m.url, "https://")
			domain = strings.TrimPrefix(domain, "http://")
			domain = strings.SplitN(domain, "/", 2)[0]
		}

		registries = append(registries, Registry{
			Name: m.name, IP: mirrorIP, URL: m.url, Domain: domain, Type: "mirror",
		})
	}

	doc := RegistryFile{
		Shared:         sharedEnabled == "true",
		TLS:            tlsEnabled == "true",
		Port:           regPort,
		Network:        RegistryNetworkInfo{Name: netName, CIDR: netCIDR},
		ProjectNetwork: projectNetwork,
		Registries:     registries,
	}

	jsonPath := filepath.Join(domainDir, ".registries.json")
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return "", err
	}
	if err := os.WriteFile(jsonPath, buf.Bytes(), 0o644); err != nil {
		return "", err
	}

	// Export scalars for external consumers (libs/image, Tilt, tests).
	os.Setenv("LOK8S_REGISTRY_JSON", jsonPath)
	os.Setenv("LOK8S_REGISTRY_SHARED", sharedEnabled)
	os.Setenv("LOK8S_REGISTRY_TLS", tlsEnabled)
	os.Setenv("LOK8S_REGISTRY_PORT", fmt.Sprint(regPort))
	os.Setenv("LOK8S_REGISTRY_NETWORK", netName)
	os.Setenv("LOK8S_REGISTRY_NETWORK_CIDR", netCIDR)
	os.Setenv("LOK8S_REGISTRY_IP_BUILD", buildIP)
	os.Setenv("LOK8S_REGISTRY_IP_CACHE", cacheIP)
	for _, r := range registries {
		if r.Type != "mirror" {
			continue
		}
		envName := "LOK8S_REGISTRY_IP_" + strings.ToUpper(strings.ReplaceAll(r.Name, "-", "_"))
		os.Setenv(envName, r.IP)
	}
	return jsonPath, nil
}

// ── Query helpers ─────────────────────────────────────────
//
// Like the bash (one jq per query), these READ THE FILE each time: tests and
// tooling mutate the JSON between calls, and the reconcile must observe the
// mutation, not a cached parse.

// loadRegistryFile parses a .registries.json.
func loadRegistryFile(path string) (*RegistryFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc RegistryFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// regFile loads the active registry JSON (bash: the LOK8S_REGISTRY_JSON
// global). Errors mirror the registry::each missing-file message.
func regFile() (*RegistryFile, error) {
	path := getenv("LOK8S_REGISTRY_JSON")
	if path == "" {
		return nil, fmt.Errorf("error: .registries.json not found (run registry::config_generate first)")
	}
	return loadRegistryFile(path)
}

// get returns a single field of a named registry (bash: registry::get).
func (f *RegistryFile) get(name, field string) string {
	for _, r := range f.Registries {
		if r.Name != name {
			continue
		}
		switch field {
		case "name":
			return r.Name
		case "ip":
			return r.IP
		case "url":
			return r.URL
		case "domain":
			return r.Domain
		case "host":
			return r.Host
		case "type":
			return r.Type
		}
	}
	return ""
}

// url builds the client-facing URL for a registry IP, honoring TLS mode
// (bash: registry::url). In TLS mode the port (443) is implicit for both
// http(s) clients and containerd, so it is omitted to match how
// `docker push <ip>/...` addresses the registry; in plain mode :80 is
// likewise implicit.
func (f *RegistryFile) url(ip string) string {
	if f.TLS {
		return "https://" + ip
	}
	return "http://" + ip
}

// containerFor resolves the Docker container name + network for a registry
// (bash: registry::container).
func (f *RegistryFile) containerFor(name string) (containerName, network string) {
	var reg *Registry
	for i := range f.Registries {
		if f.Registries[i].Name == name {
			reg = &f.Registries[i]
			break
		}
	}
	if f.Shared && reg != nil && reg.Type == "mirror" {
		return SharedRegistryPrefix + name, f.Network.Name
	}
	return f.ProjectNetwork + "-registry-" + name, f.ProjectNetwork
}
