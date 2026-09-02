package lo

// render.go — kind config rendering (YAML/TOML generation) + containerd
// certs.d + the apiserver OIDC auth-config file
// (.lok8s/drivers/lo/utils/render.sh). The rendered kind config is
// BYTE-IDENTICAL to the bash heredocs — pinned by the bash-rendered goldens
// in testdata/ — including the no-OIDC guarantee: without spec.oidc the
// output has not a single differing byte.
//
// NOTE: the driver's cluster/config.yaml file is DOCUMENTATION-ONLY — the
// config is rendered natively here, never via envsubst over that file.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kernpilot/lok8s/internal/oidc"
	"github.com/kernpilot/lok8s/internal/ui"
)

// renderKindConfig renders the full kind Cluster config (bash:
// lo::render_kind_config). Node counts / host ports / extra mounts come from
// the env the read pass exported, exactly like the bash globals.
func (d *Driver) renderKindConfig(clusterName, k8sVersion, network, clusterYAML string) string {
	_ = network // the bash accepted it for symmetry; only the env export uses it
	nodesYAML := d.renderNodes(k8sVersion, clusterYAML)
	return fmt.Sprintf(`# Rendered kind config for %s
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: %s
networking:
  disableDefaultCNI: true
  podSubnet: "%s"
  serviceSubnet: "%s"
%s
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri".registry]
      config_path = "/etc/containerd/certs.d"
    [plugins."io.containerd.grpc.v1.cri".containerd]
      max_concurrent_downloads = %s
`, clusterName, clusterName, DefaultPodCIDR, DefaultSvcCIDR, nodesYAML,
		envOr("LOK8S_MAX_CONCURRENT_DOWNLOADS", "3"))
}

// renderCertsDMount emits the certs.d extraMount entry (bash:
// lo::render_certs_d_mount — heredoc output with the trailing newline the
// callers' $(…) substitution strips).
func renderCertsDMount(hostPath string) string {
	return fmt.Sprintf(`      - hostPath: %s
        containerPath: /etc/containerd/certs.d
        readOnly: true`, hostPath)
}

// renderNodes renders the nodes: section (bash: lo::render_nodes). The
// domain comes from the DOMAIN_NAME env exactly like the bash
// (`${DOMAIN_NAME:-lok8s.dev}`) — lo main exports it before dispatch.
func (d *Driver) renderNodes(k8sVersion, clusterYAML string) string {
	var b strings.Builder
	b.WriteString("nodes:")

	domain := envOr("DOMAIN_NAME", DefaultDomain)
	certsDHost := filepath.Join(d.deps.Paths.Clusters, domain, ".containerd", "certs.d")

	cpCount := atoiOr(getenv("LOK8S_CP_COUNT"), 1)
	workerCount := atoiOr(getenv("LOK8S_WORKER_COUNT"), 0)
	extraMounts := atoiOr(getenv("LOK8S_EXTRA_MOUNTS_COUNT"), 0)

	root := loadYAML(clusterYAML)

	for i := 0; i < cpCount; i++ {
		fmt.Fprintf(&b, "\n  - role: control-plane\n    image: \"kindest/node:%s\"", k8sVersion)
		if i == 0 {
			b.WriteString(`
    kubeadmConfigPatches:
      - |
        kind: InitConfiguration
        nodeRegistration:
          kubeletExtraArgs:
            node-labels: "ingress-ready=true"`)
			// spec.oidc: append the ClusterConfiguration patch so kubeadm
			// gives the apiserver static pod --authentication-config + the
			// file as an extraVolume. Guarded by oidc.Enabled — no spec.oidc
			// ⇒ nothing appended ⇒ this list is byte-identical to today.
			if oidc.Enabled() {
				b.WriteString(renderOIDCCPPatch())
			}
			if getenv("LOK8S_HOST_PORTS") == "true" {
				b.WriteString(`
    extraPortMappings:
      - containerPort: 80
        hostPort: 80
        protocol: TCP
      - containerPort: 443
        hostPort: 443
        protocol: TCP
      - containerPort: 8080
        hostPort: 8080
        protocol: TCP`)
			}
			b.WriteString(`
    extraMounts:
      - hostPath: /var/run/docker.sock
        containerPath: /var/run/docker.sock
` + renderCertsDMount(certsDHost))
			// spec.oidc: bind-mount the host auth-config file onto node 0 so
			// kubeadm's extraVolume (in the ClusterConfiguration patch above)
			// can surface it inside the apiserver pod. Guarded by
			// oidc.Enabled — no spec.oidc ⇒ no extra mount appended.
			if oidc.Enabled() {
				b.WriteString("\n" + renderOIDCMount(d.oidcAuthConfigHostPath(domain)))
			}
			// Append user-defined extraMounts from spec.nodes.extraMounts[].
			if extraMounts > 0 && clusterYAML != "" {
				mounts := yqSeq(root, "spec", "nodes", "extraMounts")
				for m := 0; m < extraMounts && m < len(mounts); m++ {
					emHost := yqRaw(mounts[m], "hostPath")
					emContainer := yqRaw(mounts[m], "containerPath")
					emReadonly := yqOr(mounts[m], "false", "readOnly")
					fmt.Fprintf(&b, "\n      - hostPath: %s\n        containerPath: %s", emHost, emContainer)
					if emReadonly == "true" {
						b.WriteString("\n        readOnly: true")
					}
				}
			}
		} else {
			b.WriteString("\n    extraMounts:\n" + renderCertsDMount(certsDHost))
		}
	}

	for i := 0; i < workerCount; i++ {
		fmt.Fprintf(&b, "\n  - role: worker\n    image: \"kindest/node:%s\"\n    extraMounts:\n%s",
			k8sVersion, renderCertsDMount(certsDHost))
	}

	return b.String()
}

// CertsDCAPath is the fixed path containerd reads the registry CA from
// inside every kind node. The whole certs.d tree is bind-mounted, so a copy
// of the dev root CA placed at <certs_d>/.ca/rootCA.pem on the host appears
// here.
const CertsDCAPath = "/etc/containerd/certs.d/.ca/rootCA.pem"

// writeCertsD writes the containerd certs.d tree (bash: lo::write_certs_d).
func (d *Driver) writeCertsD(errOut io.Writer) error {
	domain := envOr("DOMAIN_NAME", DefaultDomain)
	certsD := filepath.Join(d.deps.Paths.Clusters, domain, ".containerd", "certs.d")

	// Refresh the certs.d tree IN PLACE — do NOT remove the directory
	// itself. A kind node bind-mounts this dir; removing it gives it a new
	// inode while the running node's mount still points at the deleted one,
	// so the node sees an EMPTY certs.d → containerd falls back to HTTPS:443
	// (the registry serves HTTP:80) → ImagePullBackOff on every re-`lo up`.
	// Clearing the contents (mindepth 1) drops stale host entries while
	// keeping the dir's inode, so an existing node mount stays valid.
	if err := os.MkdirAll(certsD, 0o755); err != nil {
		return err
	}

	// Self-protect: the whole .containerd tree is regenerated here on every
	// `lo up`/provision from cluster node IPs + the local dev CA — derived
	// state whose ENTRIES must never be committed. Drop a gitignore so
	// consumer projects don't need a hand-maintained root rule (ignore
	// everything, but keep `!.gitignore` so the file is committable and the
	// protection travels with the repo — a bare `*` would ignore itself, so
	// it could never be staged). Rewrite when the sentinel is absent so a
	// pre-fix `*`-only file upgrades, but leave a conforming file untouched.
	containerdDir := filepath.Dir(certsD)
	gitignore := filepath.Join(containerdDir, ".gitignore")
	if !gitignoreConforming(gitignore) {
		content := `# Auto-generated by lok8s — containerd registry config, regenerated every
# ` + "`lo up`" + `/provision from cluster node IPs + the local CA. The generated entries
# are never committed; this .gitignore is kept so the ignore travels with it.
*
!.gitignore
`
		if err := os.WriteFile(gitignore, []byte(content), 0o644); err != nil {
			return err
		}
	}

	// find "${certs_d}" -mindepth 1 -delete — clear entries, keep the dir.
	if entries, err := os.ReadDir(certsD); err == nil {
		for _, e := range entries {
			os.RemoveAll(filepath.Join(certsD, e.Name()))
		}
	}

	rf, err := regFile()
	if err != nil {
		return err
	}

	// TLS mode: containerd connects over HTTPS and verifies the registry
	// cert against the local dev root CA. Copy the CA into the certs.d tree
	// so each hosts.toml can reference it via the bind mount. Plain mode
	// keeps the HTTP + skip_verify behavior (no CA needed).
	tls := 0
	scheme := "http"
	if rf.TLS {
		tls = 1
		scheme = "https"
		// Resolve the shared dev CA the way mkcert (and the cert: generator)
		// do (binary-free): $CAROOT, else the XDG/OS data dir + /mkcert. The
		// registry cert is signed by this CA; `lo trust` installs it.
		caroot := getenv("CAROOT")
		if caroot == "" {
			data := getenv("XDG_DATA_HOME")
			if data == "" {
				data = getenv("HOME") + "/.local/share"
			}
			caroot = data + "/mkcert"
		}
		caSrc := filepath.Join(caroot, "rootCA.pem")
		if raw, err := os.ReadFile(caSrc); err == nil {
			if err := os.MkdirAll(filepath.Join(certsD, ".ca"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(certsD, ".ca", "rootCA.pem"), raw, 0o644); err != nil {
				return err
			}
		} else {
			fmt.Fprintln(errOut, "warning: registry TLS enabled but the local dev CA was not found at")
			fmt.Fprintf(errOut, "         %s; containerd pulls will fail cert verification. Run 'lo trust'.\n", caSrc)
		}
	}

	for _, r := range rf.Registries {
		if r.IP == "" {
			continue
		}

		hostname := ""
		capabilities := `["pull", "resolve"]`

		switch {
		case r.Host != "":
			// Framework registry (build/cache) — canonical hostname, allow push.
			hostname = r.Host
			capabilities = `["pull", "resolve", "push"]`
		case r.Domain != "":
			// Mirror with known upstream domain.
			hostname = r.Domain
		case r.URL != "":
			// Fallback: derive hostname from URL.
			hostname = strings.TrimPrefix(r.URL, "https://")
			hostname = strings.TrimPrefix(hostname, "http://")
			hostname = strings.SplitN(hostname, "/", 2)[0]
			if !hostnameRe.MatchString(hostname) {
				fmt.Fprintf(errOut, "warning: skipping mirror '%s' — unsafe hostname '%s'\n", r.Name, hostname)
				continue
			}
		default:
			continue
		}

		// Emit the per-host trust line: a CA reference in TLS mode, or
		// skip_verify for plain HTTP. The server URL omits the port — in
		// both modes containerd uses the scheme's default (80 / 443), which
		// matches the registry's listen addr and the host's push target.
		trustLine := "  skip_verify = true"
		if tls == 1 {
			trustLine = `  ca = "` + CertsDCAPath + `"`
		}

		hostsTOML := func(comment string) string {
			return fmt.Sprintf(`# Auto-generated by lok8s — %s for %s
server = "%s://%s"

[host."%s://%s"]
  capabilities = %s
%s
`, comment, r.Name, scheme, r.IP, scheme, r.IP, capabilities, trustLine)
		}

		if err := os.MkdirAll(filepath.Join(certsD, hostname), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(certsD, hostname, "hosts.toml"),
			[]byte(hostsTOML("registry mirror")), 0o644); err != nil {
			return err
		}

		if err := os.MkdirAll(filepath.Join(certsD, r.IP), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(certsD, r.IP, "hosts.toml"),
			[]byte(hostsTOML("direct IP entry")), 0o644); err != nil {
			return err
		}
	}

	ui.Debugf(errOut, "wrote containerd certs.d at %s (tls=%d)", certsD, tls)
	return nil
}

var hostnameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// gitignoreConforming reports whether the certs.d .gitignore already carries
// the `!.gitignore` sentinel (bash: grep -qFx '!.gitignore').
func gitignoreConforming(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "!.gitignore" {
			return true
		}
	}
	return false
}

// OIDCAuthConfigNodePath is the fixed node path the apiserver
// StructuredAuthenticationConfiguration is bind-mounted to inside CP node 0,
// and the path the kube-apiserver static pod reads it from (kubeadm
// extraVolume mountPath). Single source of truth for both the extraMount
// (node level) and the ClusterConfiguration patch (apiserver pod).
const OIDCAuthConfigNodePath = "/etc/kubernetes/oidc/auth-config.yaml"

// oidcAuthConfigHostPath is where the rendered auth-config lives on the host
// (bash: lo::oidc_auth_config_host_path).
func (d *Driver) oidcAuthConfigHostPath(domain string) string {
	return filepath.Join(d.deps.Paths.Clusters, domain, ".oidc", "auth-config.yaml")
}

// writeOIDCAuthConfig renders spec.oidc into the host auth-config file,
// refreshed IN PLACE (bash: lo::write_oidc_auth_config).
//
// Like writeCertsD, do NOT remove the .oidc dir: CP node 0 bind-mounts the
// FILE inside it, and replacing the dir's inode would leave the running
// node's mount pointing at a deleted path. The file is truncated+rewritten
// in place (O_TRUNC keeps the inode) so an existing node mount keeps seeing
// fresh content.
func (d *Driver) writeOIDCAuthConfig(domain string, errOut io.Writer) error {
	if domain == "" {
		domain = envOr("DOMAIN_NAME", DefaultDomain)
	}
	oidcDir := filepath.Join(d.deps.Paths.Clusters, domain, ".oidc")
	authConfig := filepath.Join(oidcDir, "auth-config.yaml")

	if !oidc.Enabled() {
		// spec.oidc absent/incomplete: leave nothing behind, but if a stale
		// file exists from a prior oidc-enabled run, clear its contents in
		// place (keep the inode for any live mount) so a disabled cluster
		// doesn't keep wiring.
		if fileExists(authConfig) {
			return os.Truncate(authConfig, 0)
		}
		return nil
	}

	if err := os.MkdirAll(oidcDir, 0o755); err != nil {
		return err
	}
	rendered, err := renderAuthConfig(errOut)
	if err != nil {
		ui.Errorf(errOut, "failed to render apiserver authentication config from spec.oidc")
		return err
	}
	// os.WriteFile opens O_TRUNC on the existing file — truncate-then-write
	// keeps the file's inode stable for a live node mount.
	if err := os.WriteFile(authConfig, []byte(rendered+"\n"), 0o644); err != nil {
		return err
	}
	ui.Debugf(errOut, "wrote apiserver authentication config at %s", authConfig)
	return nil
}

// renderOIDCCPPatch renders the CP-node-0 kubeadm patch that wires the
// kube-apiserver to the OIDC StructuredAuthenticationConfiguration (bash:
// lo::render_oidc_cp_patch): a ClusterConfiguration patch
// (apiServer.extraArgs + apiServer.extraVolumes) so the apiserver STATIC POD
// both gets the --authentication-config flag AND has the host file mounted
// into the pod. The node-level extraMount (rendered separately in
// renderNodes) only gets the file onto the node filesystem; kubeadm's
// extraVolume is what surfaces it inside the apiserver pod.
//
// kubeadm API shape VERIFIED against the v1beta4 config reference (the
// version bundled with kindest/node:v1.35.x):
//   - apiVersion: kubeadm.k8s.io/v1beta4
//   - apiServer.extraArgs is a LIST of {name,value} (NOT a map) — this
//     CHANGED in v1beta4 (was map[string]string in v1beta3).
//   - apiServer.extraVolumes entries are {name,hostPath,mountPath,readOnly,pathType}.
//     https://kubernetes.io/docs/reference/config-api/kubeadm-config.v1beta4/
//
// Emitted with a leading newline so it appends cleanly under the existing
// kubeadmConfigPatches list on node 0. The host path is the bind-mount
// target INSIDE the node (OIDCAuthConfigNodePath), not the host fs path.
func renderOIDCCPPatch() string {
	return fmt.Sprintf(`
      - |
        kind: ClusterConfiguration
        apiVersion: kubeadm.k8s.io/v1beta4
        apiServer:
          extraArgs:
            - name: authentication-config
              value: %s
          extraVolumes:
            - name: oidc-auth-config
              hostPath: %s
              mountPath: %s
              readOnly: true
              pathType: File`, OIDCAuthConfigNodePath, OIDCAuthConfigNodePath, OIDCAuthConfigNodePath)
}

// renderOIDCMount renders the node-0 extraMount entry that binds the host
// auth-config file to the fixed node path (bash: lo::render_oidc_mount).
func renderOIDCMount(hostPath string) string {
	return fmt.Sprintf(`      - hostPath: %s
        containerPath: %s
        readOnly: true`, hostPath, OIDCAuthConfigNodePath)
}

// renderAuthConfig emits the AuthenticationConfiguration YAML from the
// LOK8S_SPEC_OIDC_* env (the port of .lok8s/utils/oidc.sh
// oidc::render_auth_config; internal/oidc owns only the spec→env load).
//
// apiVersion `apiserver.config.k8s.io/v1` is the STABLE
// AuthenticationConfiguration version on the v1.35 target — VERIFIED
// empirically against the kube-apiserver v1.35.5 binary.
//
// audienceMatchPolicy is intentionally OMITTED: per the apiserver config
// schema it is only required when `audiences` has MORE THAN ONE entry. We
// always emit a single audience (the clientID), for which the field must be
// left unset (setting MatchAny with a single audience is rejected by the
// apiserver).
func renderAuthConfig(errOut io.Writer) (string, error) {
	issuer := getenv(oidc.EnvIssuer)
	if issuer == "" {
		return "", fmt.Errorf("oidc: no issuer configured")
	}
	clientID := getenv(oidc.EnvClientID)
	if clientID == "" {
		ui.Errorf(errOut, "spec.oidc.clientID is required when spec.oidc is set")
		return "", fmt.Errorf("oidc: no clientID configured")
	}

	usernameClaim := envOr(oidc.EnvUsernameClaim, "sub")
	usernamePrefix := envOr(oidc.EnvUsernamePrefix, "oidc:")
	groupsClaim := envOr(oidc.EnvGroupsClaim, "groups")
	groupsPrefix := envOr(oidc.EnvGroupsPrefix, "oidc:")
	caBundle := getenv(oidc.EnvCABundle)

	// Defensive validation at the system boundary: the issuer is an
	// external, operator-supplied URL that lands in a config file the
	// apiserver trusts. Require https (OIDC discovery + token verification
	// must not ride plain HTTP).
	if !strings.HasPrefix(issuer, "https://") {
		ui.Errorf(errOut, "spec.oidc.issuer must be an https:// URL, got '%s'", issuer)
		return "", fmt.Errorf("oidc: non-https issuer")
	}

	var b strings.Builder
	b.WriteString("# Rendered by lok8s from spec.oidc — apiserver StructuredAuthenticationConfiguration.\n")
	b.WriteString("# apiserver.config.k8s.io/v1 — verified accepted by kube-apiserver v1.35.5.\n")
	b.WriteString("apiVersion: apiserver.config.k8s.io/v1\n")
	b.WriteString("kind: AuthenticationConfiguration\n")
	b.WriteString("jwt:\n")
	b.WriteString("  - issuer:\n")
	fmt.Fprintf(&b, "      url: \"%s\"\n", issuer)
	b.WriteString("      audiences:\n")
	fmt.Fprintf(&b, "        - \"%s\"\n", clientID)
	// certificateAuthority holds the PEM bundle inline (not a path). Only
	// emitted when a caBundle was supplied; otherwise system trust.
	if caBundle != "" {
		b.WriteString("      certificateAuthority: |\n")
		for _, line := range strings.Split(caBundle, "\n") {
			fmt.Fprintf(&b, "        %s\n", line)
		}
	}
	b.WriteString("    claimMappings:\n")
	b.WriteString("      username:\n")
	fmt.Fprintf(&b, "        claim: \"%s\"\n", usernameClaim)
	// prefix is REQUIRED by the schema when claim is set (may be empty
	// string). A literal "-" means "no prefix" in k8s OIDC semantics —
	// preserved as-is so the apiserver applies its documented behavior.
	fmt.Fprintf(&b, "        prefix: \"%s\"\n", usernamePrefix)
	b.WriteString("      groups:\n")
	fmt.Fprintf(&b, "        claim: \"%s\"\n", groupsClaim)
	fmt.Fprintf(&b, "        prefix: \"%s\"", groupsPrefix)
	return b.String(), nil
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
