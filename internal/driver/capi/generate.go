package capi

// generate.go — the Go port of .lok8s/drivers/capi/generate: provider
// detection, CAPI resource generation from the whitelist-envsubst templates,
// credential-Secret management, and cluster-readiness polling.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

// TemplateVars is CAPI_TEMPLATE_VARS: the template variables Generate feeds
// to the whitelist envsubst. ONE list — it is the value set AND the
// substitution whitelist (the bash also used it as the subshell cleanup
// set; the Go render never touches the process env, so containment is
// structural). They used to be two hand-maintained copies, and a variable
// exported but missing from the whitelist ships its `${PLACEHOLDER}`
// verbatim into an applied manifest.
//
// The list is checked BOTH WAYS against the templates Generate actually
// renders — see generate_test.go (the port of tests/unit/capi_test.bats's
// drift gates). Eight placeholders in the tree (`AWS_*`,
// `HROBOT_SSH_KEY_NAME`, `ROOT_DEVICE_WWN`, `HOST_SERVER_NUMBER`,
// `MACHINE_TEMPLATE_NAME`) are deliberately untracked: they live in
// `providers/aws/` and `providers/hetzner/hrobot-machine-template.yaml`,
// which this driver does not render yet; wiring either one in means adding
// its variables here, and the gate says so by name.
var TemplateVars = []string{
	"CLUSTER_NAME", "CLUSTER_NAMESPACE", "CLUSTER_DOMAIN",
	"K8S_VERSION", "K8S_VERSION_TRIMMED", "K8S_VERSION_MINOR",
	"CP_REPLICAS", "CP_TYPE", "CREDENTIAL_SECRET_NAME",
	"HCLOUD_REGION", "HCLOUD_SSH_KEY_NAME", "HCLOUD_IMAGE_NAME", "HCLOUD_NETWORK_ENABLED",
	"POOL_NAME", "POOL_REPLICAS", "POOL_TYPE",
}

// whitelisted restricts a value set to TemplateVars — the ONE list really
// is the substitution whitelist (not just documentation): a value carried
// outside the list must NOT substitute, exactly the bash contract where
// the export set and the envsubst SHELL-FORMAT string were derived from
// the same array.
func whitelisted(vars map[string]string) map[string]string {
	out := make(map[string]string, len(TemplateVars))
	for _, name := range TemplateVars {
		if v, ok := vars[name]; ok {
			out[name] = v
		}
	}
	return out
}

// DetectProvider ports capi::detect_provider → provider::detect: explicit
// spec.provider.name wins; legacy specs are inferred from spec.hcloud /
// spec.aws (`yq -e` presence: exists and not null/false).
func (d *Driver) DetectProvider(clusterYAML string) (string, error) {
	spec := loadSpec(clusterYAML)
	if name := spec.or("", "spec", "provider", "name"); name != "" {
		return name, nil
	}
	if spec.present("spec", "hcloud") {
		return "hetzner", nil
	}
	if spec.present("spec", "aws") {
		return "aws", nil
	}
	ui.Errorf(d.stderr(), "No provider found in cluster spec: %s", clusterYAML)
	return "", fmt.Errorf("capi: no provider in %s", clusterYAML)
}

// credentialSecretName is the chained default the bash read in one yq call:
// `.spec.credentials.secretName // .spec.provider.credentials.secretRef //
// (.metadata.name + "-credentials")`.
func credentialSecretName(spec specDoc) string {
	if v := spec.or("", "spec", "credentials", "secretName"); v != "" {
		return v
	}
	if v := spec.or("", "spec", "provider", "credentials", "secretRef"); v != "" {
		return v
	}
	return spec.raw("metadata", "name") + "-credentials"
}

// Generate ports capi::generate: render the CAPI resources from the
// templates using values from the cluster spec. Returns the multi-document
// YAML stream (the bash printed it to stdout).
//
// Templates are modelled on CAPH v1.1.7's hcloud "ubuntu" flavor (v1beta2
// CAPI core + v1beta1 CAPH, kubeadm stack installed on a stock image via
// cloud-init). Substitution uses the explicit whitelist so the cloud-init's
// own shell variables ($ARCH, $RUNC, $CONTAINERD, $KUBERNETES_VERSION, …)
// pass through untouched — only the TemplateVars above are expanded.
func (d *Driver) Generate(clusterYAML, provider string) (string, error) {
	stderr := d.stderr()
	tmplDir, _, err := assets.Resolve(d.deps.Paths, "drivers/capi/cluster")
	if err != nil {
		return "", err
	}
	core := filepath.Join(tmplDir, "core")
	prov := filepath.Join(tmplDir, "providers", "hetzner")

	if info, err := os.Stat(tmplDir); err != nil || !info.IsDir() {
		ui.Errorf(stderr, "CAPI template directory not found: %s", tmplDir)
		return "", fmt.Errorf("capi: template directory not found: %s", tmplDir)
	}

	if provider != "hetzner" {
		ui.Errorf(stderr, "CAPI provider '%s' is not supported yet (only 'hetzner').", provider)
		return "", fmt.Errorf("capi: provider %q not supported", provider)
	}

	spec := loadSpec(clusterYAML)
	pgEnabled := spec.or("false", "spec", "provider", "config", "placementGroups")

	// Cluster identity + Kubernetes version (full, trimmed, and minor — the
	// cloud-init needs the trimmed/minor forms for the apt repo + package
	// pins). Held in a LOCAL map, never the process env — see envsubstMap.
	k8sVersion := spec.raw("spec", "kubernetes", "version")
	trimmed := strings.TrimPrefix(k8sVersion, "v") // v1.31.12 -> 1.31.12
	minor := trimmed                               // 1.31.12  -> 1.31
	if i := strings.LastIndex(trimmed, "."); i >= 0 {
		minor = trimmed[:i]
	}
	vars := map[string]string{
		"CLUSTER_NAME":           spec.raw("metadata", "name"),
		"CLUSTER_NAMESPACE":      spec.or("default", "spec", "cluster", "namespace"),
		"CLUSTER_DOMAIN":         spec.raw("spec", "cluster", "domain"),
		"K8S_VERSION":            k8sVersion,
		"K8S_VERSION_TRIMMED":    trimmed,
		"K8S_VERSION_MINOR":      minor,
		"CP_REPLICAS":            spec.or("1", "spec", "controlPlane", "replicas"),
		"CP_TYPE":                spec.or("cax11", "spec", "controlPlane", "type"),
		"CREDENTIAL_SECRET_NAME": credentialSecretName(spec),
		"HCLOUD_REGION": func() string {
			if v := spec.or("", "spec", "provider", "config", "region"); v != "" {
				return v
			}
			return spec.or("fsn1", "spec", "hcloud", "region")
		}(),
		"HCLOUD_SSH_KEY_NAME": func() string {
			if v := spec.or("", "spec", "provider", "config", "sshKeyName"); v != "" {
				return v
			}
			return spec.or("", "spec", "hcloud", "sshKeyName")
		}(),
		"HCLOUD_IMAGE_NAME":      spec.or("ubuntu-24.04", "spec", "provider", "config", "image"),
		"HCLOUD_NETWORK_ENABLED": spec.or("false", "spec", "provider", "config", "network", "enabled"),
	}

	if vars["HCLOUD_SSH_KEY_NAME"] == "" {
		ui.Errorf(stderr, "spec.provider.config.sshKeyName is required for the hetzner CAPI provider")
		return "", fmt.Errorf("capi: spec.provider.config.sshKeyName is required")
	}

	renderOne := func(path string) (string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(envsubstMap(raw, whitelisted(vars))), nil
	}

	var b strings.Builder
	first := true
	for _, tpl := range []string{
		filepath.Join(core, "cluster.yaml"),
		filepath.Join(prov, "hetzner-cluster.yaml"),
		filepath.Join(core, "kubeadm-control-plane.yaml"),
		filepath.Join(prov, "hcloud-machine-template-controlplane.yaml"),
	} {
		if !first {
			b.WriteString("---\n")
		}
		first = false
		out, err := renderOne(tpl)
		if err != nil {
			return "", err
		}
		b.WriteString(out)
	}

	// One MachineDeployment + KubeadmConfigTemplate + worker machine
	// template per pool, names iterated in document order (the bash
	// while-read over spec::pool_names — never word-split, so a
	// whitespace-bearing pool name reaches the validator whole).
	for _, pool := range spec.poolNames() {
		if pool == "" {
			continue
		}
		// A bad pool name FAILS the whole render instead of emitting a
		// partial stream: the dispatch runs the driver under a disabled
		// errexit, so an unguarded render once flowed on as a control plane
		// with no workers (capi_test.bats pins this).
		if !validatePoolName(pool, stderr) {
			return "", fmt.Errorf("capi: invalid pool name %q", pool)
		}
		vars["POOL_NAME"] = pool
		vars["POOL_REPLICAS"] = spec.poolField(pool, "replicas", "1")
		vars["POOL_TYPE"] = spec.poolField(pool, "type", "cax11")
		for _, tpl := range []string{
			filepath.Join(core, "machine-deployment.yaml"),
			filepath.Join(prov, "kubeadm-config-template.yaml"),
			filepath.Join(prov, "hcloud-machine-template-worker.yaml"),
		} {
			b.WriteString("---\n")
			out, err := renderOne(tpl)
			if err != nil {
				return "", err
			}
			b.WriteString(out)
		}
	}

	// The bash captured the stream with $( … ), which strips trailing
	// newlines, then printed it back with one `printf '%s\n'`.
	rendered := strings.TrimRight(b.String(), "\n")
	if rendered == "" {
		ui.Errorf(stderr, "the CAPI manifest stream rendered EMPTY from %s — refusing to continue", tmplDir)
		return "", fmt.Errorf("capi: manifest stream rendered empty from %s", tmplDir)
	}

	// Optional anti-affinity: opt-in `spread` placement groups (control
	// plane + workers). A spread group caps at 10 servers, so this is OFF
	// by default — always-on would make a cluster with >10 nodes in a group
	// fail.
	if pgEnabled == "true" {
		injected, err := injectPlacementGroups(rendered)
		if err != nil {
			return "", err
		}
		rendered = injected
	}
	return rendered + "\n", nil
}

// EnsureCredentialsSecret ports capi::ensure_credentials: create or update
// the provider credential Secret on the management cluster.
func (d *Driver) EnsureCredentialsSecret(ctx context.Context, clusterYAML, provider, kubeconfig string) error {
	stderr := d.stderr()
	spec := loadSpec(clusterYAML)
	secretName := credentialSecretName(spec)
	namespace := spec.or("default", "spec", "cluster", "namespace")

	if err := requireCredentials(provider, stderr); err != nil {
		return err
	}

	var createArgs []string
	switch provider {
	case "hetzner":
		createArgs = []string{
			"create", "secret", "generic", secretName,
			"--namespace", namespace,
			"--kubeconfig", kubeconfig,
			"--from-literal=hcloud-token=" + os.Getenv("HCLOUD_TOKEN"),
			"--from-literal=robot-user=" + os.Getenv("HROBOT_USER"),
			"--from-literal=robot-password=" + os.Getenv("HROBOT_PASSWORD"),
			"--dry-run=client", "-o", "yaml",
		}
	case "aws":
		// bash: `: "${AWS_REGION:?AWS_REGION required for AWS provider}"` —
		// the ${:?} expansion aborts the shell; here it is a plain error.
		if os.Getenv("AWS_REGION") == "" {
			ui.Errorf(stderr, "AWS_REGION required for AWS provider")
			return fmt.Errorf("capi: AWS_REGION required for AWS provider")
		}
		createArgs = []string{
			"create", "secret", "generic", secretName,
			"--namespace", namespace,
			"--kubeconfig", kubeconfig,
			"--from-literal=access-key-id=" + os.Getenv("AWS_ACCESS_KEY_ID"),
			"--from-literal=secret-access-key=" + os.Getenv("AWS_SECRET_ACCESS_KEY"),
			"--from-literal=region=" + os.Getenv("AWS_REGION"),
			"--dry-run=client", "-o", "yaml",
		}
	default:
		ui.Errorf(stderr, "Unsupported provider for credentials: %s", provider)
		return fmt.Errorf("capi: unsupported provider for credentials: %s", provider)
	}

	// bash: `kubectl create … | kubectl apply -f -` — a PIPELINE, so only
	// the apply's status decides (this bash function predates the rendered-
	// into-a-variable namespace guard in driver::provision and deliberately
	// keeps the pipe: a failed create feeds apply an empty stream, and the
	// apply's own failure is what surfaces). The create's error is ignored
	// here for the same reason.
	var manifest strings.Builder
	_ = d.deps.Runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: createArgs, Stdout: &manifest})
	return d.deps.Runner.Run(ctx, execx.Cmd{
		Name:  "kubectl",
		Args:  []string{"apply", "--kubeconfig", kubeconfig, "-f", "-"},
		Stdin: strings.NewReader(manifest.String()),
	})
}

// requireCredentials ports credentials::require: env-var presence per
// provider, all missing vars reported before failing.
func requireCredentials(provider string, stderr io.Writer) error {
	var missing []string
	switch provider {
	case "hetzner":
		if os.Getenv("HCLOUD_TOKEN") == "" {
			missing = append(missing, "HCLOUD_TOKEN")
		}
	case "aws":
		if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
			missing = append(missing, "AWS_ACCESS_KEY_ID")
		}
		if os.Getenv("AWS_SECRET_ACCESS_KEY") == "" {
			missing = append(missing, "AWS_SECRET_ACCESS_KEY")
		}
	default:
		ui.Errorf(stderr, "unknown provider '%s' for credential check", provider)
		return fmt.Errorf("capi: unknown provider %q for credential check", provider)
	}
	if len(missing) > 0 {
		for _, v := range missing {
			ui.Errorf(stderr, "required environment variable %s is not set", v)
		}
		return fmt.Errorf("capi: missing credentials: %s", strings.Join(missing, ", "))
	}
	return nil
}

// WaitReady ports capi::wait_ready: poll the Cluster CR's .status.phase
// until "Provisioned" or timeout. namespace "" defaults to "default",
// timeoutSeconds <= 0 to 600 (the bash parameter defaults).
func (d *Driver) WaitReady(ctx context.Context, kubeconfig, clusterName, namespace string, timeoutSeconds int) error {
	stderr := d.stderr()
	if namespace == "" {
		namespace = "default"
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 600
	}
	const interval = 10

	ui.Debugf(stderr, "Waiting for CAPI cluster %s to become ready (timeout: %ds)", clusterName, timeoutSeconds)

	for elapsed := 0; elapsed < timeoutSeconds; elapsed += interval {
		var out strings.Builder
		err := d.deps.Runner.Run(ctx, execx.Cmd{
			Name: "kubectl",
			Args: []string{
				"get", "cluster", clusterName,
				"--namespace", namespace,
				"--kubeconfig", kubeconfig,
				"-o", "jsonpath={.status.phase}",
			},
			Stdout: &out,
			Stderr: io.Discard,
		})
		phase := strings.TrimRight(out.String(), "\n")
		if err != nil {
			phase = ""
		}
		if phase == "Provisioned" {
			ui.Debugf(stderr, "Cluster %s is Provisioned", clusterName)
			return nil
		}
		if phase == "" {
			phase = "Unknown"
		}
		ui.Debugf(stderr, "Cluster %s phase: %s (%d/%ds)", clusterName, phase, elapsed, timeoutSeconds)
		d.sleepSeconds(interval)
	}

	ui.Errorf(stderr, "Timed out waiting for cluster %s (%ds)", clusterName, timeoutSeconds)
	return fmt.Errorf("capi: timed out waiting for cluster %s", clusterName)
}
