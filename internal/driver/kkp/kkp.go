// Package kkp is the Go port of the KKP driver (.lok8s/drivers/kkp/{main,
// api}): managed clusters via Kubermatic Kubernetes Platform. KKP hosts the
// control plane on a seed cluster; users only pay for worker nodes. All
// operations use the KKP REST API via the api.go client (curl behind the
// execx.Runner seam).
//
// Work directory: clusters/<domain>/.kkp/ (cluster_id + project_id).
// Kubeconfig: .kubeconfig/<metadata.name>.yaml (framework convention shared
// with the other drivers; bootstrap expects it there).
//
// Required env vars: KKP_TOKEN, KKP_API_URL (or spec.kkp.apiUrl)
// Optional env vars: HCLOUD_TOKEN (Hetzner), AWS_ACCESS_KEY_ID (AWS)
package kkp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/ui"
)

// Name is the driver's registry name (spec `kind: Kkp`).
const Name = "kkp"

func init() {
	driver.Register(Name, func(deps *driver.Deps) (driver.Driver, error) {
		return New(deps), nil
	})
}

// Driver is the KKP driver. Implements driver.Driver. No Hooks: unlike
// kubeone/capi it calls no kubehz/bootstrap libs — everything it needs
// (spec readers, credentials, the REST client) is ported natively.
type Driver struct {
	deps *driver.Deps

	// sleep/now are the wait seams (tests stub them; prod = time.Sleep /
	// time.Now). The bash wait loops measure elapsed wall time via
	// `date +%s`, so the clock is a seam too.
	sleep func(time.Duration)
	now   func() time.Time
}

// New builds the driver over its dispatch-provided dependencies.
func New(deps *driver.Deps) *Driver {
	return &Driver{deps: deps, sleep: time.Sleep, now: time.Now}
}

func (d *Driver) stderr() io.Writer {
	if d.deps.Stderr != nil {
		return d.deps.Stderr
	}
	return os.Stderr
}

func (d *Driver) sleepSeconds(n int) { d.sleep(time.Duration(n) * time.Second) }

func (d *Driver) clusterYAML(domain string) string {
	return filepath.Join(d.deps.Paths.Clusters, domain, "cluster.lok8s.yaml")
}

func (d *Driver) workDir(domain string) string {
	return filepath.Join(d.deps.Paths.Clusters, domain, ".kkp")
}

// readIDFile reads a saved work-dir handle like the bash `$(cat file)` —
// trailing newlines stripped, ok=false when the file is unreadable.
func readIDFile(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimRight(string(raw), "\n"), true
}

// exportAPIURLFromSpec mirrors the `if [[ -z "${KKP_API_URL:-}" ]]; then
// export KKP_API_URL=$(yq -r '.spec.kkp.apiUrl' …)` block: the BARE read —
// a spec without the field exports the literal "null", which the HTTPS
// validation then rejects (the port keeps that surface, not a cleaned-up
// empty string).
func exportAPIURLFromSpec(spec specDoc) {
	if os.Getenv("KKP_API_URL") == "" {
		os.Setenv("KKP_API_URL", spec.raw("spec", "kkp", "apiUrl"))
	}
}

// ── Driver contract ───────────────────────────────────────

// Provision provisions a KKP cluster (bash: driver::provision).
func (d *Driver) Provision(ctx context.Context, domain string) error {
	stderr := d.stderr()
	cy := d.clusterYAML(domain)
	workDir := d.workDir(domain)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}

	// 1. Validate credentials.
	if err := d.validateCredentials(cy); err != nil {
		return err
	}

	// 2. Extract config from cluster spec.
	spec := loadSpec(cy)
	clusterName := spec.raw("metadata", "name")
	projectID := spec.raw("spec", "kkp", "projectId")
	datacenter := spec.raw("spec", "kkp", "datacenter")
	k8sVersion := spec.raw("spec", "kubernetes", "version")
	// provider may be an object ({name: hetzner}) or a bare scalar.
	provider := spec.providerName("hetzner")

	// Use spec URL if KKP_API_URL not set in environment.
	exportAPIURLFromSpec(spec)
	if err := d.validateURL(os.Getenv("KKP_API_URL"), stderr); err != nil {
		return err
	}

	// 3. Idempotent re-run: reuse a previously created cluster if it still
	// exists.
	clusterID := ""
	if saved, ok := readIDFile(filepath.Join(workDir, "cluster_id")); ok {
		clusterID = saved
		// bash: kkp::get_cluster … >/dev/null 2>&1 — fully suppressed probe.
		if _, err := d.getCluster(ctx, projectID, clusterID, io.Discard); err == nil {
			ui.Debugf(stderr, "KKP cluster %s already exists — skipping create", clusterID)
		} else {
			ui.Warnf(stderr, "Saved cluster ID %s no longer exists in KKP — creating a new cluster", clusterID)
			clusterID = ""
		}
	}

	// 4. Create cluster.
	if clusterID == "" {
		clusterJSON, err := buildClusterJSON(clusterName, k8sVersion, datacenter, provider, spec, stderr)
		if err != nil {
			// DELIBERATE DEVIATION, fail-loud: the bash `cluster_json=$(…)`
			// was unguarded under a disabled errexit, so an unsupported
			// provider printed its error and then POSTed a mangled payload
			// the server rejected. Here the same error message aborts before
			// the wire.
			return err
		}
		clusterID, err = d.createCluster(ctx, projectID, clusterJSON)
		if err != nil {
			return err
		}

		// Persist cluster ID for later operations (bash: echo > file — one
		// trailing newline).
		if err := os.WriteFile(filepath.Join(workDir, "cluster_id"), []byte(clusterID+"\n"), 0o644); err != nil { // #nosec G306 -- an identifier, not a credential
			return err
		}
		if err := os.WriteFile(filepath.Join(workDir, "project_id"), []byte(projectID+"\n"), 0o644); err != nil { // #nosec G306 -- an identifier, not a credential
			return err
		}
		ui.Debugf(stderr, "KKP cluster ID %s saved to %s/cluster_id", clusterID, workDir)
	}

	// 5. Wait for cluster to reach Running phase.
	if err := d.waitReady(ctx, projectID, clusterID, 0); err != nil {
		return err
	}

	// 6. Create machine deployments (worker pools).
	if err := d.createWorkerPools(ctx, projectID, clusterID, spec, provider); err != nil {
		return err
	}

	// 7. Fetch and store kubeconfig (named by metadata.name — framework
	// convention shared with the other drivers; bootstrap expects it there).
	kubeconfigPath := filepath.Join(d.deps.Paths.Base, ".kubeconfig", clusterName+".yaml")
	if err := d.getKubeconfig(ctx, projectID, clusterID, kubeconfigPath); err != nil {
		return err
	}

	ui.Debugf(stderr, "KKP cluster %s provisioned successfully", clusterName)
	return nil
}

// Destroy tears down a KKP cluster (bash: driver::destroy).
func (d *Driver) Destroy(ctx context.Context, domain string) error {
	stderr := d.stderr()
	cy := d.clusterYAML(domain)
	workDir := d.workDir(domain)

	// Read saved cluster/project IDs.
	clusterID, okC := readIDFile(filepath.Join(workDir, "cluster_id"))
	projectID, okP := readIDFile(filepath.Join(workDir, "project_id"))
	if !okC || !okP {
		ui.Errorf(stderr, "No saved cluster ID found in %s/cluster_id", workDir)
		ui.Errorf(stderr, "Cannot destroy cluster without a cluster ID")
		return fmt.Errorf("kkp: no saved cluster ID in %s", workDir)
	}

	// Set API URL if not in environment.
	spec := loadSpec(cy)
	exportAPIURLFromSpec(spec)
	if err := d.validateURL(os.Getenv("KKP_API_URL"), stderr); err != nil {
		return err
	}

	// Delete the cluster via the KKP API.
	//
	// This used to be `|| warn "…continuing cleanup"`, and the cleanup
	// below then removed the work dir — which holds cluster_id. Read with
	// the guard at the top of this function ("Cannot destroy cluster
	// without a cluster ID"), that made a transient API failure PERMANENT:
	// the user cluster kept running and billing, `lo down` reported success
	// because the tail of this function cannot fail, and the only handle
	// capable of retrying the delete had just been deleted. The dispatch
	// runs the driver's destroy with its error captured, so nothing else
	// would have caught it (issue #91's class,
	// kkp_capi_destroy_guards_test.bats pins this).
	//
	// Local cleanup is still deliberately tolerant of a failed REMOTE call
	// — it just may not happen after one, and may not claim success it did
	// not have.
	if err := d.deleteCluster(ctx, projectID, clusterID); err != nil {
		ui.Errorf(stderr, "KKP cluster delete FAILED (cluster %s, project %s)", clusterID, projectID)
		ui.Errorf(stderr, "  the cluster is still running and still billing")
		ui.Errorf(stderr, "  KEEPING %s — cluster_id there is the only handle a retry has", workDir)
		ui.Errorf(stderr, "  retry with 'lo down', or delete the cluster in the KKP UI")
		return fmt.Errorf("kkp: cluster delete failed: %w", err)
	}

	// Clean up local state (kubeconfig is named by metadata.name).
	clusterName := spec.raw("metadata", "name")
	_ = os.Remove(filepath.Join(d.deps.Paths.Base, ".kubeconfig", clusterName+".yaml"))
	_ = os.RemoveAll(workDir)

	ui.Debugf(stderr, "KKP cluster %s destroyed and local state cleaned", clusterID)
	return nil
}

// Status reports the cluster status word (bash: driver::status). The v2
// REST API exposes no .status.phase — status is derived from the cluster's
// existence plus the /health endpoint (see coreHealthy).
func (d *Driver) Status(ctx context.Context, domain string) (string, error) {
	cy := d.clusterYAML(domain)
	workDir := d.workDir(domain)

	// Check if cluster was ever provisioned.
	clusterID, okC := readIDFile(filepath.Join(workDir, "cluster_id"))
	projectID, okP := readIDFile(filepath.Join(workDir, "project_id"))
	if !okC || !okP {
		return "NotFound", nil
	}

	// Set API URL if not in environment (no HTTPS validation HERE — the
	// api client enforces it per call, its diagnostics suppressed below).
	exportAPIURLFromSpec(loadSpec(cy))

	if _, err := d.getCluster(ctx, projectID, clusterID, io.Discard); err != nil {
		return "Unknown", nil
	}

	healthJSON, err := d.health(ctx, projectID, clusterID, io.Discard)
	if err != nil {
		return "Unknown", nil
	}

	if coreHealthy(healthJSON) {
		return "Running", nil
	}
	return "Provisioning", nil
}

// Kubeconfig returns the standard kubeconfig path (bash:
// driver::kubeconfig).
func (d *Driver) Kubeconfig(ctx context.Context, domain string) (string, error) {
	spec := loadSpec(d.clusterYAML(domain))
	return filepath.Join(d.deps.Paths.Base, ".kubeconfig", spec.raw("metadata", "name")+".yaml"), nil
}

// EnsureCredentials is the optional bash contract function
// driver::ensure_credentials: validate credentials before provisioning
// starts (setting KKP_API_URL from the spec first when needed — the
// TOLERANT `// ""` read here, unlike provision's bare one).
func (d *Driver) EnsureCredentials(ctx context.Context, clusterYAML string) error {
	if os.Getenv("KKP_API_URL") == "" {
		if apiURL := loadSpec(clusterYAML).or("", "spec", "kkp", "apiUrl"); apiURL != "" {
			os.Setenv("KKP_API_URL", apiURL)
		}
	}
	return d.validateCredentials(clusterYAML)
}

// ── Internal helpers ──────────────────────────────────────

// jsonObj is an insertion-ordered JSON object, so the marshaled payloads
// are byte-identical to the bash `jq -n` builders (goldens in
// testdata/kkp_payloads.golden pin them).
type jsonObj struct {
	keys []string
	vals map[string]any
}

func obj(pairs ...any) *jsonObj {
	o := &jsonObj{vals: map[string]any{}}
	for i := 0; i+1 < len(pairs); i += 2 {
		o.set(pairs[i].(string), pairs[i+1])
	}
	return o
}

func (o *jsonObj) set(key string, val any) *jsonObj {
	if _, dup := o.vals[key]; !dup {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = val
	return o
}

func (o *jsonObj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		vb, err := json.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// marshalJQ renders with jq's default formatting (2-space indent), matching
// the `jq -n` output the bash handed to curl.
func marshalJQ(v any) (string, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// buildClusterJSON ports _build_cluster_json: the cluster creation payload.
func buildClusterJSON(name, version, datacenter, provider string, spec specDoc, stderr io.Writer) (string, error) {
	preset := spec.or("", "spec", "kkp", "preset")

	cloudSpec, err := buildCloudSpec(provider, preset, stderr)
	if err != nil {
		return "", err
	}

	// jq: cloud: ({dc: $dc} + $cloud) — dc first, then the provider keys.
	cloud := obj("dc", datacenter)
	for _, k := range cloudSpec.keys {
		cloud.set(k, cloudSpec.vals[k])
	}
	return marshalJQ(obj(
		"cluster", obj(
			"name", name,
			"spec", obj(
				"version", version,
				"cloud", cloud,
			),
		),
	))
}

// buildCloudSpec ports _build_cloud_spec: the cloud section per provider.
func buildCloudSpec(provider, preset string, stderr io.Writer) (*jsonObj, error) {
	if preset != "" {
		return obj(provider, obj(), "presetName", preset), nil
	}
	switch provider {
	case "hetzner":
		return obj("hetzner", obj("token", os.Getenv("HCLOUD_TOKEN"))), nil
	case "aws":
		return obj("aws", obj(
			"accessKeyID", os.Getenv("AWS_ACCESS_KEY_ID"),
			"secretAccessKey", os.Getenv("AWS_SECRET_ACCESS_KEY"),
		)), nil
	case "byo", "bringyourown":
		// No cloud credentials, no machine-controller — control plane only.
		// Workers join out-of-band (kubeadm token); pairs with a
		// `bringyourown` datacenter in the Seed.
		return obj("bringyourown", obj()), nil
	default:
		ui.Errorf(stderr, "Unsupported KKP provider: %s", provider)
		return nil, fmt.Errorf("kkp: unsupported provider: %s", provider)
	}
}

// createWorkerPools ports _create_worker_pools: machine deployments from
// spec.workers.
func (d *Driver) createWorkerPools(ctx context.Context, projectID, clusterID string, spec specDoc, provider string) error {
	stderr := d.stderr()

	if spec.poolCount() == 0 {
		ui.Debugf(stderr, "No worker pools defined in spec")
		return nil
	}

	// MD creation 503s until the provider components are ready — gate on
	// them only when there are pools to create (byo clusters never have
	// them).
	if err := d.waitComponents(ctx, projectID, clusterID, kkpWaitTimeout(),
		"machineController", "operatingSystemManager"); err != nil {
		return err
	}

	// Idempotent re-run: skip pools that already exist. Errors tolerated
	// (bash: `… 2>/dev/null | jq … 2>/dev/null || existing_pools=""`).
	existing := map[string]bool{}
	if listJSON, err := d.listMachineDeployments(ctx, projectID, clusterID, io.Discard); err == nil {
		var mds []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(listJSON), &mds) == nil {
			for _, md := range mds {
				existing[md.Name] = true
			}
		}
	}

	// Names iterated whole, in document order (the bash while-read — never
	// `for … in $(yq …)`, whose word-splitting broke a whitespace-bearing
	// pool name into fragments BEFORE validation).
	for _, pool := range spec.poolNames() {
		if pool == "" {
			continue
		}
		if !validatePoolName(pool, stderr) {
			return fmt.Errorf("kkp: invalid pool name %q", pool)
		}

		if existing[pool] {
			ui.Debugf(stderr, "Worker pool %s already exists — skipping create", pool)
			continue
		}

		replicas := spec.poolField(pool, "replicas", "1")
		flavor := spec.poolField(pool, "flavor", "")
		if flavor == "" {
			flavor = spec.poolField(pool, "type", "")
		}
		osName := spec.poolField(pool, "operatingSystem", "ubuntu")

		if flavor == "" {
			ui.Errorf(stderr, "Worker pool '%s' has no flavor/type set", pool)
			return fmt.Errorf("kkp: worker pool %q has no flavor/type set", pool)
		}

		// Autoscaler config (optional).
		autoscalerMin := spec.poolField(pool, "autoscaler.min", "0")
		autoscalerMax := spec.poolField(pool, "autoscaler.max", "0")

		mdJSON, err := buildMachineDeploymentJSON(pool, replicas, flavor, osName, provider,
			autoscalerMin, autoscalerMax, stderr)
		if err != nil {
			return err
		}

		if _, err := d.createMachineDeployment(ctx, projectID, clusterID, mdJSON); err != nil {
			ui.Errorf(stderr, "Failed to create machine deployment: %s", pool)
			return fmt.Errorf("kkp: failed to create machine deployment %s: %w", pool, err)
		}
	}
	return nil
}

// buildMachineDeploymentJSON ports _build_machinedeployment_json.
func buildMachineDeploymentJSON(name, replicas, flavor, osName, provider,
	autoscalerMin, autoscalerMax string, stderr io.Writer) (string, error) {

	var cloudSpec *jsonObj
	switch provider {
	case "hetzner":
		// REST HetznerNodeSpec field is `type` (machine-controller's
		// internal rawConfig calls it serverType — do not confuse the two).
		cloudSpec = obj("hetzner", obj("type", flavor))
	case "aws":
		cloudSpec = obj("aws", obj("instanceType", flavor))
	default:
		ui.Errorf(stderr, "Unsupported provider for machine deployment: %s", provider)
		return "", fmt.Errorf("kkp: unsupported provider for machine deployment: %s", provider)
	}

	// jq --argjson replicas: the spec value is spliced as a JSON NUMBER; a
	// non-numeric value made jq fail (and the bash then POSTed a mangled
	// payload) — here it fails loud before the wire, same deviation as
	// buildClusterJSON's.
	replicasNum, err := strconv.Atoi(replicas)
	if err != nil {
		ui.Errorf(stderr, "Invalid replicas for pool %s: %s (must be numeric)", name, replicas)
		return "", fmt.Errorf("kkp: invalid replicas for pool %s: %s", name, replicas)
	}

	mdSpec := obj(
		"replicas", replicasNum,
		"template", obj(
			"cloud", cloudSpec,
			"operatingSystem", obj(osName, obj()),
		),
	)

	// Add autoscaler bounds if configured. bash: `(( min > 0 )) || (( max >
	// 0 ))` — a non-numeric value is an arithmetic error under a disabled
	// errexit, read as false; the Go parse-to-0 lands in the same place.
	minN, _ := strconv.Atoi(autoscalerMin)
	maxN, _ := strconv.Atoi(autoscalerMax)
	if minN > 0 || maxN > 0 {
		mdSpec.set("minReplicas", minN)
		mdSpec.set("maxReplicas", maxN)
	}

	return marshalJQ(obj("name", name, "spec", mdSpec))
}
