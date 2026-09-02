// Package deploy is the Go port of the domain artifact deployment
// (.lok8s/libs/deploy: deploy::apply / deploy::apply_filtered /
// deploy::_apply / deploy::wait_crds — bash wins on any divergence; the
// `lo destroy` half of that file stays with the provision port).
//
// Applies the single clusters/<domain>/artifacts.yaml produced by `lo
// build`: CRDs first (server-side apply + wait Established), then the rest
// (server-side apply + scoped wait-ready). ApplyFiltered deploys only the
// subset carrying one label. Workload-plane ordering is intentionally not a
// framework primitive: kubectl handles in-manifest order, Tilt handles
// runtime deps at the resource level, cluster-infra ordering lives in
// cluster.spec.bootstrap.
//
// Every kubectl runs through the kapply.Applier (the ported kapply::apply /
// kapply::wait_ready) against the AMBIENT KUBECONFIG — the caller resolves
// it (build.ResolveKubeconfigForDomain: a deploy domain follows its
// clusterRef); no --kubeconfig flag is threaded, exactly like the bash.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kapply"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrHandled marks a failure whose [error] line was already printed.
var ErrHandled = errors.New("deploy: handled")

// ExitError carries a kubectl apply's exit status out of the unfiltered
// apply — bash's `set -e` exited the whole `lo` with that status.
type ExitError struct{ Code int }

func (e *ExitError) Error() string {
	return fmt.Sprintf("deploy: kubectl apply exit status %d", e.Code)
}

// ExitCode is the process exit status the error maps to.
func (e *ExitError) ExitCode() int { return e.Code }

var (
	labelKeyRe   = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)
	labelValueRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	// hasObjectsRe: any line with a `kind:` declaration is taken as evidence
	// of an object (bash: deploy::_has_objects). Robust enough for
	// kustomize output, which always emits a `kind:` field per doc.
	hasObjectsRe = regexp.MustCompile(`(?m)^kind:[ \t\v\f\r]+[A-Za-z]`)
	// docSepRe splits a kustomize stream on its `---` document separators.
	docSepRe = regexp.MustCompile(`(?m)^---[ \t]*$`)
)

// Deployer runs the apply phases. Applier carries the runner + writers.
type Deployer struct {
	Paths   *config.Paths
	Applier *kapply.Applier
	// Stderr receives the [error]/[warn]/[debug] lines (default os.Stderr).
	Stderr io.Writer
}

func (d *Deployer) stderr() io.Writer {
	if d.Stderr != nil {
		return d.Stderr
	}
	return os.Stderr
}

// HasObjects reports whether a YAML stream contains at least one kubernetes
// object (bash: deploy::_has_objects). Empty streams (zero docs, comments
// only, whitespace, or only `---` separators) report false — used to skip
// `kubectl apply` gracefully, which fails with "error: no objects passed to
// apply" on an empty stream (a label-filtered subset that excludes
// everything is a legitimate workflow).
func HasObjects(stream string) bool {
	return hasObjectsRe.MatchString(stream)
}

// ParseLabel splits the -l/--label value into key and value (bash:
// main::deploy's ${label%%=*} / ${label#*=} guard). Prints the [error] line
// and returns ErrHandled on a missing '=', an empty key or an empty value.
func ParseLabel(stderr io.Writer, label string) (key, value string, err error) {
	key, value, found := strings.Cut(label, "=")
	if !found || key == "" || value == "" {
		ui.Errorf(stderr, "invalid --label '%s' — expected key=value (e.g. lok8s.dev/name=zitadel)", label)
		return "", "", ErrHandled
	}
	return key, value, nil
}

// Apply applies the domain's single artifact (bash: deploy::apply). Bash's
// errexit is ACTIVE on this path: the first failing kubectl apply exits the
// whole `lo` with kubectl's status — returned here as *ExitError.
func (d *Deployer) Apply(ctx context.Context, domain string) error {
	if domain == "" {
		domain = os.Getenv("DOMAIN_NAME")
		if domain == "" {
			domain = "lok8s.dev"
		}
	}
	artifact := filepath.Join(d.Paths.Clusters, domain, "artifacts.yaml")
	if !fileExists(artifact) {
		ui.Errorf(d.stderr(), "no artifact for %s: %s — run 'lo build' first", domain, artifact)
		return ErrHandled
	}
	ui.Debugf(d.stderr(), "Deploying %s from %s", domain, artifact)
	return d.applyFile(ctx, domain, artifact, true)
}

// ApplyFiltered applies only the resources in the domain artifact carrying
// <labelKey>=<labelValue> (bash: deploy::apply_filtered). The key is used
// verbatim: a bare key (app) or a namespaced one (lok8s.dev/name).
// Selective deploy is user opt-in — it requires kustomize `labels:` (or
// metadata.labels) on the targets to address.
//
// Bash runs the subset apply as `deploy::_apply … || rc=$?`, which SUSPENDS
// errexit inside it: a failing kubectl apply there is logged and the phase
// sequence continues, and the function's status is the scoped wait's
// (always 0). Reproduced as-is — bash wins — see the package README note
// in the port report.
func (d *Deployer) ApplyFiltered(ctx context.Context, domain, labelKey, labelValue string) error {
	// Validate key/value (the bash interpolated them into a yq expression).
	if !labelKeyRe.MatchString(labelKey) || !labelValueRe.MatchString(labelValue) {
		ui.Errorf(d.stderr(), "Invalid label selector: key and value must be alphanumeric with . _ - (key may also contain /)")
		return ErrHandled
	}
	artifact := filepath.Join(d.Paths.Clusters, domain, "artifacts.yaml")
	if !fileExists(artifact) {
		ui.Errorf(d.stderr(), "no artifact for %s: %s — run 'lo build' first", domain, artifact)
		return ErrHandled
	}

	// Filter the single domain artifact by the (full) label key = value into
	// a temp file — the apply streams from a file, and the artifact can be
	// large. Cleanup on every return path.
	raw, err := os.ReadFile(artifact)
	if err != nil {
		ui.Warnf(d.stderr(), "no objects match %s=%s in %s", labelKey, labelValue, artifact)
		return nil
	}
	subset := selectDocs(string(raw), func(doc *yaml.Node) bool {
		lbl := lookup(lookup(lookup(doc, "metadata"), "labels"), labelKey)
		// yq `==` compares a scalar's TEXT: an unquoted `true` or `1` label
		// matches "true"/"1" (verified against yq v4.53).
		return lbl != nil && lbl.Kind == yaml.ScalarNode && lbl.Tag != "!!null" && lbl.Value == labelValue
	})
	if !HasObjects(subset) {
		ui.Warnf(d.stderr(), "no objects match %s=%s in %s", labelKey, labelValue, artifact)
		return nil
	}
	tmp, err := os.CreateTemp("", "lok8s-deploy-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(subset); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	ui.Debugf(d.stderr(), "Deploying %s subset (%s=%s)", domain, labelKey, labelValue)
	return d.applyFile(ctx, fmt.Sprintf("%s (%s=%s)", domain, labelKey, labelValue), tmp.Name(), false)
}

// applyFile applies a manifest FILE in two phases (bash: deploy::_apply).
// Empty streams are a graceful no-op.
//
//	Phase 1: extract + apply CustomResourceDefinitions, then wait for them
//	         to become Established. Server-side apply — large CRDs (CNPG,
//	         CAPI) exceed the client-side last-applied annotation limit.
//	Phase 2: apply the whole stream (re-applying the CRDs server-side is a
//	         no-op), then wait for the stream's own Deployments/DaemonSets/
//	         StatefulSets to become ready (scoped to the manifest). Best-
//	         effort — a readiness timeout is a ⚠, never fatal.
//
// abortOnError is the errexit state of the calling bash path: active on
// deploy::apply (a failing apply exits with kubectl's status), suspended on
// deploy::apply_filtered (failures are logged, the sequence continues).
func (d *Deployer) applyFile(ctx context.Context, label, file string, abortOnError bool) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	manifest := string(raw)
	if !HasObjects(manifest) {
		ui.Debugf(d.stderr(), "No objects for %s, skipping apply", label)
		return nil
	}

	// Phase 1: CRDs first (subset small enough to stay a string).
	crds := selectDocs(manifest, func(doc *yaml.Node) bool {
		return scalar(lookup(doc, "kind")) == "CustomResourceDefinition"
	})
	if HasObjects(crds) {
		ui.Debugf(d.stderr(), "Applying CRDs for %s", label)
		// bash: `echo "${crds}" | kapply::apply` — echo appends a newline.
		if _, rc := d.Applier.Apply(ctx, label+" crds", crds+"\n"); rc != 0 && abortOnError {
			return &ExitError{Code: rc}
		}
		d.waitCRDs(ctx, crds)
	}

	// Phase 2: apply the rest + scoped wait-ready — both from the file.
	ui.Debugf(d.stderr(), "Applying resources for %s", label)
	if _, rc := d.Applier.Apply(ctx, label, manifest); rc != 0 && abortOnError {
		return &ExitError{Code: rc}
	}
	_ = d.Applier.WaitReady(ctx, label, 120, manifest)
	return nil
}

// waitCRDs waits for each CRD in the stream to become Established (bash:
// deploy::wait_crds): `kubectl wait --for=condition=Established
// crd/<name> --timeout=60s` per name, stderr discarded, a failure is a
// warning. kubectl's stdout ("… condition met") passes through.
func (d *Deployer) waitCRDs(ctx context.Context, crds string) {
	for _, doc := range splitDocs(crds) {
		var n yaml.Node
		if yaml.Unmarshal([]byte(doc), &n) != nil {
			continue
		}
		name := scalar(lookup(lookup(deref(&n), "metadata"), "name"))
		if name == "" {
			continue
		}
		ui.Debugf(d.stderr(), "Waiting for CRD: %s", name)
		err := d.Applier.Runner.Run(ctx, execx.Cmd{
			Name:   "kubectl",
			Args:   []string{"wait", "--for=condition=Established", "crd/" + name, "--timeout=60s"},
			Stdout: d.Applier.Stdout,
			Stderr: io.Discard,
		})
		if err != nil {
			ui.Warnf(d.stderr(), "CRD %s not established within timeout", name)
		}
	}
}

// splitDocs splits a YAML stream on its document separators, dropping
// docs with no content (bash: yq's document iteration).
func splitDocs(stream string) []string {
	var docs []string
	for _, doc := range docSepRe.Split(stream, -1) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		docs = append(docs, doc)
	}
	return docs
}

// selectDocs is `yq 'select(<pred>)'` over the stream: the RAW text of every
// document whose parsed root satisfies pred, joined by `---` separators.
// Raw text (not a re-marshal) so the applied bytes are the artifact's own.
func selectDocs(stream string, pred func(doc *yaml.Node) bool) string {
	var kept []string
	for _, doc := range splitDocs(stream) {
		var n yaml.Node
		if yaml.Unmarshal([]byte(doc), &n) != nil {
			continue
		}
		root := deref(&n)
		if root == nil || !pred(root) {
			continue
		}
		kept = append(kept, strings.TrimSuffix(strings.TrimPrefix(doc, "\n"), "\n"))
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n---\n")
}

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

func scalar(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return ""
	}
	return n.Value
}
