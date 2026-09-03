// Package inventory is the Go port of the ClusterInventory writer
// (.lok8s/libs/inventory/main — bash wins on any divergence).
//
// `lo` records WHAT it deployed into the cluster itself: a cluster-scoped
// SINGLETON ClusterInventory (lok8s.dev/v1alpha1) named "cluster"
// (OpenShift-ClusterVersion-style spec/status split). lo owns .spec
// (framework/driver/k8s versions, the sha256 of cluster.lok8s.yaml, and the
// resolved spec.bootstrap addon set with chart versions + categories); the
// in-cluster agent owns .status via the status subresource — lo only defines
// that schema, it never writes status.
//
// STRICTLY metadata: the CRD schema enumerates every field and carries no
// x-kubernetes-preserve-unknown-fields, so chart values / env overrides /
// credentials structurally cannot land in the inventory.
//
// Publish is FAIL-SOFT BY CONTRACT: an unreachable cluster, a missing
// kubeconfig, a CRD conflict, RBAC — none of it may ever break a
// provision/bootstrap. It warns and returns; the signature has no error
// return so the contract holds at the type level (provision.Hooks and
// bootstrap.Dispatcher pin it).
package inventory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/bootstrap"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

const (
	// APIVersion is the ClusterInventory API group/version.
	APIVersion = "lok8s.dev/v1alpha1"
	// CRDName is the ClusterInventory CRD's metadata.name.
	CRDName = "clusterinventories.lok8s.dev"
)

// CR is the ClusterInventory resource, field order = the bash jq program's
// key order (jq preserves insertion order; the optional provider /
// kubernetesVersion keys are ADDED after addons, hence last).
type CR struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

// Metadata is the CR's metadata block.
type Metadata struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

// Spec is the lo-owned .spec.
type Spec struct {
	Lok8sVersion      string  `json:"lok8sVersion"`
	Kind              string  `json:"kind"`
	SpecHash          string  `json:"specHash"`
	RenderedAt        string  `json:"renderedAt"`
	Addons            []Addon `json:"addons"`
	Provider          string  `json:"provider,omitempty"`
	KubernetesVersion string  `json:"kubernetesVersion,omitempty"`
}

// Addon is one resolved spec.bootstrap entry — names + version/category
// metadata only; inline values/env never reach this struct. appVersion is
// not tracked by the khelm chart.yaml pins today, so it is never emitted —
// the schema keeps the field for the agent / future metadata.
type Addon struct {
	Name         string `json:"name"`
	Source       string `json:"source"`
	ChartVersion string `json:"chartVersion,omitempty"`
	Category     string `json:"category,omitempty"`
}

// CRDManifest resolves the generated ClusterInventory CRD manifest (bash:
// inventory::_crd_manifest). Two homes for the same generated artifact
// (single source: operator/crds/schema/, drift-gated by `lo crds check`):
// consumer repos vendor only .lok8s/** (b env sync), so the mirror inside
// .lok8s is the one guaranteed present; the lok8s repo itself additionally
// has operator/crds/.
func CRDManifest(p *config.Paths) (string, bool) {
	// The .lok8s mirror resolves through the asset resolver: the project's
	// copy when present, else the embedded CRD (ejected on first use).
	if c, _, err := assets.Resolve(p, "libs/inventory/manifests/clusterinventory.crd.yaml"); err == nil && fileExists(c) {
		return c, true
	}
	if c := filepath.Join(p.Base, "operator", "crds", "clusterinventory.yaml"); fileExists(c) {
		return c, true
	}
	return "", false
}

// now is the UTC RFC-3339 timestamp (bash: inventory::_now). Honors
// SOURCE_DATE_EPOCH (reproducible builds / deterministic tests); a
// non-numeric value reads as epoch 0, like bash's printf %()T does.
func now() string {
	if v := os.Getenv("SOURCE_DATE_EPOCH"); v != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			n = 0
		}
		return time.Unix(n, 0).UTC().Format("2006-01-02T15:04:05Z")
	}
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

// Build resolves the full ClusterInventory CR (bash: inventory::build_json
// — PURE, no cluster access). Resolves spec.bootstrap through the SAME
// shared parser the apply path uses (bootstrap.ResolveEntries / ParseEntry —
// map-form entries stay intact, per-driver defaults included) and reads each
// addon's pinned chart version + lok8s.dev/category label. Only entry NAMES
// + version/category metadata are emitted — inline values/env never leave
// this function. Errors are printed (bash error()) and returned.
func Build(p *config.Paths, stderr io.Writer, domainName, clusterYAML string) (*CR, error) {
	if !fileExists(clusterYAML) {
		ui.Errorf(stderr, "inventory: cluster spec not found: %s", clusterYAML)
		return nil, fmt.Errorf("inventory: cluster spec not found: %s", clusterYAML)
	}

	// The fallback covers "no kind declared". It must NOT cover a malformed
	// kind — it is reported into the ClusterInventory CR and read back by
	// the agent, so defaulting it to "lo" would publish a false driver
	// identity.
	kind, err := domain.SpecDriver(clusterYAML, "lo")
	if err != nil {
		ui.Errorf(stderr, "inventory: cluster spec declares a malformed kind: %s", clusterYAML)
		return nil, fmt.Errorf("inventory: malformed kind in %s: %w", clusterYAML, err)
	}

	raw, err := os.ReadFile(clusterYAML)
	if err != nil {
		ui.Errorf(stderr, "inventory: cluster spec not found: %s", clusterYAML)
		return nil, err
	}
	var root yaml.Node
	_ = yaml.Unmarshal(raw, &root) // an unparsable spec reads as empty (yq // "")
	spec := lookup(deref(&root), "spec")
	provider := scalarOrEmpty(lookup(lookup(spec, "provider"), "name"))
	k8sVersion := scalarOrEmpty(lookup(lookup(spec, "kubernetes"), "version"))
	// Strip a kindest-node @sha256 digest suffix (bash: ${k8s_version%%@*}).
	k8sVersion, _, _ = strings.Cut(k8sVersion, "@")

	// The version is the binary's (ldflags, else the embedded VERSION) —
	// nothing reads .lok8s/VERSION from disk any more.
	lok8sVersion := assets.Version()
	sum := sha256.Sum256(raw)

	cr := &CR{
		APIVersion: APIVersion,
		Kind:       "ClusterInventory",
		Metadata:   Metadata{Name: "cluster", Labels: map[string]string{"lok8s.dev/domain": domainName}},
		Spec: Spec{
			Lok8sVersion:      lok8sVersion,
			Kind:              kind,
			SpecHash:          hex.EncodeToString(sum[:]),
			RenderedAt:        now(),
			Addons:            []Addon{},
			Provider:          provider,
			KubernetesVersion: k8sVersion,
		},
	}

	// bash: mapfile < <(bootstrap::_resolve_entries … 2>/dev/null) — a
	// failed resolve is an empty entry list.
	entries, _ := bootstrap.ResolveEntries(clusterYAML, kind)
	for _, e := range entries {
		if e == "" {
			continue
		}
		// bash: bootstrap::_parse_entry … 2>/dev/null — its diagnostics are
		// suppressed here; the deploy already failed loudly on it upstream.
		parsed, err := bootstrap.ParseEntry(p, io.Discard, domainName, e)
		if err != nil {
			// Say so instead of silently thinning the inventory. Log only the
			// entry's NAME/key: map-form entries carry inline values/env, which
			// must not reach (CI-captured) logs from this path.
			ui.Warnf(stderr, "inventory: skipping unparseable bootstrap entry '%s'", entryKey(e))
			continue
		}
		a := Addon{Name: parsed.Name, Source: "target"}
		if parsed.Builtin {
			a.Source = "addon"
		}
		if v := chartVersion(parsed.Dir); v != "-" {
			a.ChartVersion = v
		}
		if c := category(parsed.Dir); c != "-" {
			a.Category = c
		}
		cr.Spec.Addons = append(cr.Spec.Addons, a)
	}
	return cr, nil
}

// BuildJSON renders Build's CR the way the bash `jq -n` program printed it
// (2-space indent, no HTML escaping, no trailing newline — the value a
// `cr=$(…)` substitution held).
func BuildJSON(p *config.Paths, stderr io.Writer, domainName, clusterYAML string) (string, error) {
	cr, err := Build(p, stderr, domainName, clusterYAML)
	if err != nil {
		return "", err
	}
	return cr.JSON(), nil
}

// JSON renders the CR as jq-style pretty JSON.
func (c *CR) JSON() string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// entryKey is the bash `jq -r 'if type == "object" then (keys[0] // "?")
// else tostring end'` over a compact-JSON entry — "?" when unparsable.
func entryKey(entry string) string {
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(entry), &n); err != nil {
		return "?"
	}
	v := deref(&n)
	if v == nil {
		return "?"
	}
	if v.Kind == yaml.MappingNode {
		if len(v.Content) < 2 {
			return "?"
		}
		// jq's keys are SORTED; keys[0] is the smallest.
		keys := make([]string, 0, len(v.Content)/2)
		for i := 0; i+1 < len(v.Content); i += 2 {
			keys = append(keys, v.Content[i].Value)
		}
		sort.Strings(keys)
		return keys[0]
	}
	if v.Kind == yaml.ScalarNode {
		if v.Tag == "!!null" {
			return "null"
		}
		return v.Value
	}
	// A bare array entry: jq tostring prints its compact JSON form.
	return entry
}

// chartVersion reads the pinned chart version of a khelm addon (bash:
// addons::_version — `yq -r '.version // "-"'` when chart.yaml exists, "-"
// otherwise). A yq failure (unparsable chart.yaml) printed nothing → "".
func chartVersion(dir string) string {
	chart := filepath.Join(dir, "chart.yaml")
	if !fileExists(chart) {
		return "-"
	}
	raw, err := os.ReadFile(chart)
	if err != nil {
		return ""
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return ""
	}
	v := lookup(deref(&root), "version")
	if v == nil || v.Kind != yaml.ScalarNode || v.Tag == "!!null" || (v.Tag == "!!bool" && v.Value == "false") {
		return "-"
	}
	return v.Value
}

// categoryRe is the grep -oE pattern of addons::_category.
var categoryRe = regexp.MustCompile(`lok8s\.dev/category:[ \t\v\f\r]*[a-zA-Z0-9-]+`)

// category is the addon's lok8s.dev/category label (bash: addons::_category
// — `grep -rhoE … | LC_ALL=C sort | head -1`, then everything up to the
// colon dropped and whitespace stripped; "-" when none). Like `grep -r`,
// symlinks below the directory are not followed.
func category(dir string) string {
	var matches []string
	root, err := os.Stat(dir)
	if err == nil && root.IsDir() {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			matches = append(matches, categoryRe.FindAllString(string(data), -1)...)
			return nil
		})
	}
	if len(matches) == 0 {
		return "-"
	}
	sort.Strings(matches) // byte order = LC_ALL=C
	line := matches[0]
	cat := line[strings.LastIndex(line, ":")+1:]
	cat = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\v' || r == '\f' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, cat)
	if cat == "" {
		return "-"
	}
	return cat
}

// Publish applies the ClusterInventory CRD (idempotent) + the CR spec
// (server-side apply, field manager "lok8s") to the cluster (bash:
// inventory::publish). FAIL-SOFT: every failure path warns and returns —
// publishing the inventory must never break a provision/deploy. Skips
// cleanly when there is no cluster spec (deploy domains) or no reachable
// cluster.
func Publish(ctx context.Context, p *config.Paths, r execx.Runner, stderr io.Writer, domainName, clusterYAML, kubeconfig string) {
	// Deploy domains (and anything else without its own cluster.lok8s.yaml)
	// have no inventory of their own — the referenced cluster's inventory is
	// written when THAT cluster provisions/bootstraps.
	if !fileExists(clusterYAML) {
		ui.Debugf(stderr, "inventory: no cluster spec at %s — nothing to publish", clusterYAML)
		return
	}
	if !fileExists(kubeconfig) {
		ui.Warnf(stderr, "inventory: kubeconfig not found (%s) — skipping ClusterInventory publish", kubeconfig)
		return
	}

	crd, ok := CRDManifest(p)
	if !ok {
		ui.Warnf(stderr, "inventory: ClusterInventory CRD manifest not found — skipping publish")
		return
	}
	quiet := func(stdin string, args ...string) error {
		c := execx.Cmd{Name: "kubectl", Args: args, Stdout: io.Discard, Stderr: io.Discard}
		if stdin != "" {
			c.Stdin = strings.NewReader(stdin)
		}
		return r.Run(ctx, c)
	}
	if err := quiet("", "--kubeconfig", kubeconfig, "apply", "--server-side", "--field-manager=lok8s", "-f", crd); err != nil {
		ui.Warnf(stderr, "inventory: could not apply the ClusterInventory CRD (cluster unreachable, RBAC, or a conflicting CRD) — skipping publish")
		return
	}
	// Best-effort: a fresh CRD needs a moment to be Established before its
	// CRs resolve. Non-fatal — the SSA below would just warn.
	_ = quiet("", "--kubeconfig", kubeconfig, "wait", "--for=condition=Established", "crd/"+CRDName, "--timeout=30s")

	cr, err := BuildJSON(p, stderr, domainName, clusterYAML)
	if err != nil {
		ui.Warnf(stderr, "inventory: failed to build the ClusterInventory for %s — skipping publish", domainName)
		return
	}
	// bash: kubectl … -f - <<< "${cr}" — a here-string appends a newline.
	if err := quiet(cr+"\n", "--kubeconfig", kubeconfig, "apply", "--server-side", "--field-manager=lok8s", "-f", "-"); err != nil {
		ui.Warnf(stderr, "inventory: failed to publish the ClusterInventory for %s (provision/deploy unaffected)", domainName)
		return
	}
	ui.Debugf(stderr, "inventory: published ClusterInventory 'cluster' for %s", domainName)
}

// PublishHook returns Publish bound to its dependencies, in the shape of
// provision.Hooks.InventoryPublish / bootstrap.Dispatcher.InventoryPublish
// — the seam both dispatchers call as inventory::publish. No error return,
// by contract.
func PublishHook(p *config.Paths, r execx.Runner, stderr io.Writer) func(ctx context.Context, domain, clusterYAML, kubeconfig string) {
	return func(ctx context.Context, domainName, clusterYAML, kubeconfig string) {
		Publish(ctx, p, r, stderr, domainName, clusterYAML, kubeconfig)
	}
}

// ErrMalformedKind reports a spec whose .kind is present but not a bare
// driver name (never defaulted).
var ErrMalformedKind = errors.New("inventory: malformed kind")

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func deref(n *yaml.Node) *yaml.Node {
	for n != nil && (n.Kind == yaml.DocumentNode || n.Kind == yaml.AliasNode) {
		if n.Kind == yaml.DocumentNode {
			if len(n.Content) == 0 {
				return nil
			}
			n = n.Content[0]
			continue
		}
		n = n.Alias
	}
	return n
}

func lookup(n *yaml.Node, key string) *yaml.Node {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return deref(n.Content[i+1])
		}
	}
	return nil
}

// scalarOrEmpty is `yq -r '<path> // ""'` on a scalar: the raw scalar text,
// "" for a missing/null/false value or a non-scalar.
func scalarOrEmpty(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return ""
	}
	if n.Tag == "!!bool" && n.Value == "false" {
		return ""
	}
	return n.Value
}
