package cli

// lo kubeconfig — emit a kubeconfig for a domain's cluster.
// Go port of .lok8s/libs/kubeconfig; output is byte-identical.
//
// `lo kubeconfig`              print the active domain's cluster kubeconfig
// `lo kubeconfig --oidc`       print an OIDC kubeconfig: the user authenticates
//                              via the kubelogin exec plugin (kubectl
//                              oidc-login) against spec.oidc's IdP — a PUBLIC
//                              client with PKCE, NO client secret.
//
// The --oidc form reuses the SAME cluster server + CA as the resolved admin
// kubeconfig, but swaps the user for an exec credential plugin. This is the
// kubeconfig a cluster user (not the operator) runs: `kubectl` shells out to
// `kubectl oidc-login get-token`, which drives the browser auth-code+PKCE flow
// and caches the ID token. The apiserver must be wired for the matching OIDC
// issuer (spec.oidc → StructuredAuthenticationConfiguration; internal/oidc).

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/oidc"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() { registerPorted("kubeconfig", newKubeconfigCommand) }

func newKubeconfigCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var clusterOverride string
	cmd := &cobra.Command{
		Use:          "kubeconfig",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()

			// The argsh spec has NO positional (the domain rides the --domain
			// flag); a stray positional is a parse error there (rc 2, message
			// below). Same message here, exit 1 — the version/use precedent.
			if len(args) > 0 {
				return argshErrorf(stderr, "too many arguments: %s", args[0])
			}

			// -v/--verbose → DEBUG, like the argsh entrypoint (the deploy-
			// domain resolution emits a debug line).
			if v, _ := cmd.Flags().GetCount("verbose"); v > 0 {
				os.Setenv("DEBUG", "1")
			}

			// Domain: the canonical precedence chain (--domain flag >
			// DOMAIN_NAME env > clusters/.active > lok8s.dev). The argsh
			// ~domain validator does NOT run (pre-set local, same as build) —
			// an unknown domain flows through and fails at resolution below.
			domainFlag, _ := cmd.Flags().GetString("domain")
			d := domain.Resolve(domainFlag, paths.Clusters, stderr)

			// Ambient KUBECONFIG default, exactly what the argsh entrypoint
			// exported before dispatching (lo main): spec metadata.name >
			// --cluster flag > LOK8S_CLUSTER_NAME > "local".
			clusterFlag, _ := cmd.Flags().GetString("cluster")
			os.Setenv("KUBECONFIG", build.AmbientKubeconfig(paths, d, clusterFlag))

			// Resolve the cluster admin kubeconfig path for this domain
			// (follows a deploy domain's clusterRef, same as `lo deploy`).
			// Sets KUBECONFIG.
			if err := build.ResolveKubeconfigForDomain(paths, d, clusterOverride, stderr); err != nil {
				return ErrHandled
			}
			// For a cluster domain (no deploy.lok8s.yaml) the resolver is a
			// no-op (it only resolves clusterRef chains); fall back to the
			// per-cluster kubeconfig `lo` writes (<metadata.name>.yaml) under
			// .kubeconfig/.
			kubeconfig := os.Getenv("KUBECONFIG")
			if kubeconfig == "" || !fileExists(kubeconfig) {
				adminPath, ok := kubeconfigAdminPath(paths, d)
				if !ok {
					ui.Errorf(stderr, "could not resolve a kubeconfig for %s (is the cluster provisioned?)", d)
					return ErrHandled
				}
				kubeconfig = adminPath
			}
			if !fileExists(kubeconfig) {
				ui.Errorf(stderr, "kubeconfig not found: %s (provision the cluster first)", kubeconfig)
				return ErrHandled
			}

			if n, _ := cmd.Flags().GetCount("oidc"); n > 0 {
				return kubeconfigEmitOIDC(paths, d, kubeconfig, cmd.OutOrStdout(), stderr)
			}
			raw, err := os.ReadFile(kubeconfig)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(raw)
			return err
		},
	}
	f := cmd.Flags()
	// argsh 'oidc|o:+' is a counting flag; CountP keeps `-o -o` valid there
	// and here alike.
	f.CountP("oidc", "o", "Emit an OIDC (kubelogin exec-plugin) kubeconfig instead of the admin one")
	f.StringVar(&clusterOverride, "cluster-override", "", "Override cluster domain for kubeconfig resolution")
	return cmd
}

// kubeconfigClusterYAML resolves a domain's cluster spec file, following a
// deploy domain's clusterRef to the real cluster (bash:
// kubeconfig::_cluster_yaml). The clusterRef resolution failures are silent
// (bash: provision::resolve_clusterref … 2>/dev/null) — the caller owns the
// error message.
func kubeconfigClusterYAML(p *config.Paths, d string) (string, bool) {
	domainDir := filepath.Join(p.Clusters, d)
	clusterYAML := filepath.Join(domainDir, "cluster.lok8s.yaml")

	// Deploy domain → follow clusterRef to the real cluster.
	if !fileExists(clusterYAML) && fileExists(filepath.Join(domainDir, "deploy.lok8s.yaml")) {
		ref := silentClusterRef(p, d)
		if ref == "" {
			return "", false
		}
		clusterYAML = filepath.Join(p.Clusters, ref, "cluster.lok8s.yaml")
	}
	if !fileExists(clusterYAML) {
		return "", false
	}
	return clusterYAML, true
}

// silentClusterRef is provision::resolve_clusterref with stderr discarded:
// spec.clusterRef.domain, validated to point at an existing cluster domain
// carrying a cluster spec. "" on any failure.
func silentClusterRef(p *config.Paths, d string) string {
	var doc struct {
		Spec struct {
			ClusterRef struct {
				Domain string `yaml:"domain"`
			} `yaml:"clusterRef"`
		} `yaml:"spec"`
	}
	raw, err := os.ReadFile(filepath.Join(p.Clusters, d, "deploy.lok8s.yaml"))
	if err != nil || yaml.Unmarshal(raw, &doc) != nil {
		return ""
	}
	ref := doc.Spec.ClusterRef.Domain
	if ref == "" {
		return ""
	}
	if info, err := os.Stat(filepath.Join(p.Clusters, ref)); err != nil || !info.IsDir() {
		return ""
	}
	if !fileExists(filepath.Join(p.Clusters, ref, "cluster.lok8s.yaml")) {
		return ""
	}
	return ref
}

// kubeconfigAdminPath is the path `lo` writes a provisioned cluster's
// kubeconfig to (<metadata.name>.yaml under .kubeconfig/) — bash:
// kubeconfig::_admin_path. Resolves the cluster spec for the domain (or its
// clusterRef); false when no cluster spec or metadata.name is found.
func kubeconfigAdminPath(p *config.Paths, d string) (string, bool) {
	clusterYAML, ok := kubeconfigClusterYAML(p, d)
	if !ok {
		return "", false
	}
	name := specClusterName(clusterYAML)
	if name == "" {
		return "", false
	}
	return filepath.Join(p.Base, ".kubeconfig", name+".yaml"), true
}

// kubeconfigEmitOIDC emits an OIDC kubeconfig to out, reusing the cluster
// server + CA from src and the issuer/clientID from spec.oidc (bash:
// kubeconfig::emit_oidc).
func kubeconfigEmitOIDC(p *config.Paths, d, src string, out, stderr io.Writer) error {
	// The LOK8S_SPEC_OIDC_* vars are exported by the drivers' spec readers
	// during provision/bootstrap — a fresh shell running `lo kubeconfig
	// --oidc` has none of that context, so load spec.oidc from the domain's
	// cluster spec (following a deploy domain's clusterRef) when the env
	// doesn't carry it yet. Each failure gets its own error so a
	// resolution/parse problem is never masked by the generic "no usable
	// spec.oidc" below.
	if !oidc.Enabled() {
		clusterYAML, ok := kubeconfigClusterYAML(p, d)
		if !ok {
			ui.Errorf(stderr, "could not resolve a cluster spec for '%s' — cannot read spec.oidc", d)
			return ErrHandled
		}
		if err := oidc.LoadSpec(clusterYAML, stderr); err != nil {
			return ErrHandled
		}
	}

	// spec.oidc must be present to build an exec-plugin user.
	if !oidc.Enabled() {
		ui.Errorf(stderr, "domain '%s' has no usable spec.oidc (issuer + clientID required) — cannot emit an OIDC kubeconfig", d)
		return ErrHandled
	}
	issuer := os.Getenv(oidc.EnvIssuer)
	clientID := os.Getenv(oidc.EnvClientID)
	// Same boundary rule oidc::render_auth_config enforces apiserver-side: the
	// issuer lands in a kubeconfig users will authenticate against — never
	// plain HTTP.
	if !strings.HasPrefix(issuer, "https://") {
		ui.Errorf(stderr, "spec.oidc.issuer must be an https:// URL, got '%s'", issuer)
		return ErrHandled
	}

	// Pull the cluster stanza from the source kubeconfig — reuse its server +
	// CA verbatim so the OIDC kubeconfig talks to the exact same apiserver
	// endpoint (bash: four independent `yq -r … // "" 2>/dev/null` reads; a
	// parse failure reads as all-empty here as there).
	var doc struct {
		Clusters []struct {
			Name    string `yaml:"name"`
			Cluster struct {
				Server string `yaml:"server"`
				CAData string `yaml:"certificate-authority-data"`
				CAFile string `yaml:"certificate-authority"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if raw, err := os.ReadFile(src); err == nil {
		_ = yaml.Unmarshal(raw, &doc)
	}
	var clusterName, server, caData, caFile string
	if len(doc.Clusters) > 0 {
		clusterName = doc.Clusters[0].Name
		server = doc.Clusters[0].Cluster.Server
		caData = doc.Clusters[0].Cluster.CAData
		caFile = doc.Clusters[0].Cluster.CAFile
	}
	if clusterName == "" || server == "" {
		ui.Errorf(stderr, "could not read cluster server/name from %s", src)
		return ErrHandled
	}

	contextName := clusterName + "-oidc"
	const userName = "oidc"

	// Header + cluster stanza. Prefer inline CA data; fall back to a CA file
	// path.
	fmt.Fprintln(out, "apiVersion: v1")
	fmt.Fprintln(out, "kind: Config")
	fmt.Fprintln(out, "clusters:")
	fmt.Fprintf(out, "  - name: %s\n", clusterName)
	fmt.Fprintln(out, "    cluster:")
	fmt.Fprintf(out, "      server: %s\n", server)
	if caData != "" {
		fmt.Fprintf(out, "      certificate-authority-data: %s\n", caData)
	} else if caFile != "" {
		fmt.Fprintf(out, "      certificate-authority: %s\n", caFile)
	}
	fmt.Fprintln(out, "contexts:")
	fmt.Fprintf(out, "  - name: %s\n", contextName)
	fmt.Fprintln(out, "    context:")
	fmt.Fprintf(out, "      cluster: %s\n", clusterName)
	fmt.Fprintf(out, "      user: %s\n", userName)
	fmt.Fprintf(out, "current-context: %s\n", contextName)

	// The kubelogin (int128/kubelogin) exec credential plugin: kubectl invokes
	// `kubectl oidc-login get-token` (the plugin installs as
	// kubectl-oidc_login). Public client + PKCE: NO --oidc-client-secret. The
	// extra scopes pull the email/groups/profile claims the apiserver maps
	// (see spec.oidc claims). apiVersion client.authentication.k8s.io/v1 is
	// the GA exec-plugin contract (stable since k8s 1.22; valid on the 1.35
	// target).
	fmt.Fprintln(out, "users:")
	fmt.Fprintf(out, "  - name: %s\n", userName)
	fmt.Fprintln(out, "    user:")
	fmt.Fprintln(out, "      exec:")
	fmt.Fprintln(out, "        apiVersion: client.authentication.k8s.io/v1")
	fmt.Fprintln(out, "        command: kubectl")
	fmt.Fprintln(out, "        args:")
	fmt.Fprintln(out, "          - oidc-login")
	fmt.Fprintln(out, "          - get-token")
	fmt.Fprintf(out, "          - --oidc-issuer-url=%s\n", issuer)
	fmt.Fprintf(out, "          - --oidc-client-id=%s\n", clientID)
	fmt.Fprintln(out, "          - --oidc-extra-scope=email")
	fmt.Fprintln(out, "          - --oidc-extra-scope=groups")
	fmt.Fprintln(out, "          - --oidc-extra-scope=profile")
	// interactiveMode: kubelogin opens a browser; Always is the right hint for
	// a human-driven login. provideClusterInfo not needed (no cluster-info
	// reliance).
	fmt.Fprintln(out, "        interactiveMode: IfAvailable")
	return nil
}
