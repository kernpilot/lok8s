// Package lint is the Go port of .lok8s/libs/lint — structure and spec
// validation. Validates domain specs, spec.bootstrap entries (driver addons
// exist), kustomization references, label conventions, and secrets
// encryption status. Every message is byte-identical to the bash
// implementation.
package lint

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/secrets"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrPrinted marks an error whose message was already printed in the bash
// implementation's own format ([error] … on stderr).
var ErrPrinted = ui.ErrHandled // one sentinel for every package; see internal/ui

// Linter runs the lint checks against a resolved project layout.
type Linter struct {
	Paths  *config.Paths
	Out    io.Writer
	ErrOut io.Writer
}

// Run is main::lint: per-domain checks for the given domain (all domains when
// empty — unreachable through the CLI, where the resolver always yields one),
// then the repo-global checks.
func (l *Linter) Run(domain string) error {
	var domains []string
	if domain != "" {
		domains = []string{domain}
	} else {
		domains = l.listDomains()
	}

	errorCount := 0
	for _, d := range domains {
		if !l.all(d) {
			errorCount++
		}
	}

	// Repo-global checks (not per-domain): the services.yaml catalog + every
	// per-service lok8s.yaml, the one-cluster-per-apex topology, plus
	// Tiltfile/label drift. These run once regardless of how many domains
	// exist.
	if !l.services() {
		errorCount++
	}
	if !l.apex() {
		errorCount++
	}
	l.drift() // warnings only — never bumps the error count

	if errorCount != 0 {
		ui.Errorf(l.ErrOut, "%d validation error(s)", errorCount)
		return ErrPrinted
	}
	return nil
}

// listDomains lists all domain names from clusters/, excluding hidden dirs
// (bash: _list_domains in the entrypoint).
func (l *Linter) listDomains() []string {
	return sortedDirNames(l.Paths.Clusters)
}

// all runs every lint check on a single domain (bash: lint::all). Returns
// false when the domain has validation errors.
func (l *Linter) all(domain string) bool {
	domainDir := l.Paths.Clusters + "/" + domain

	fmt.Fprintf(l.Out, "Validating: %s\n", domain)

	// Check spec file exists
	clusterSpec := domainDir + "/cluster.lok8s.yaml"
	deploySpec := domainDir + "/deploy.lok8s.yaml"
	if !isFile(clusterSpec) && !isFile(deploySpec) {
		ui.Errorf(l.ErrOut, "  Missing cluster.lok8s.yaml or deploy.lok8s.yaml")
		return false
	}

	specFile := deploySpec
	if isFile(clusterSpec) {
		specFile = clusterSpec
	}

	errs := l.schema(domainDir, specFile)
	errs += l.clusterref(domainDir, specFile)
	errs += l.bootstrap(domainDir, specFile, domain)
	errs += l.kustomization(domainDir)
	// Advisory by design, and deliberately not added to `errs`: labels
	// reports a missing convention label, and secrets is documented
	// warnings-only at its own definition — as is drift at the repo-wide
	// aggregator. Their findings print; they do not decide the verdict.
	l.labels(domainDir)
	l.secrets(domainDir, domain)

	// The verdict is printed AFTER it is computed: an unconditional "  OK"
	// above this check once printed "  OK" followed by "N validation
	// error(s)" — the exit code was right, but the per-domain line an
	// operator scans was not.
	if errs != 0 {
		ui.Errorf(l.ErrOut, "%d validation error(s)", errs)
		return false
	}

	fmt.Fprintln(l.Out, "  OK")
	return true
}

// schema validates required fields: kind, apiVersion, metadata.name,
// spec.kind (cluster). Bash: lint::schema. Returns the number of errors.
func (l *Linter) schema(domainDir, specFile string) int {
	root := firstDoc(specFile)
	errs := 0

	if valueOr(nodeAt(root, "kind"), "") == "" {
		ui.Errorf(l.ErrOut, "  Missing required field: kind")
		errs++
	}
	if valueOr(nodeAt(root, "apiVersion"), "") == "" {
		ui.Errorf(l.ErrOut, "  Missing required field: apiVersion")
		errs++
	}
	if valueOr(nodeAt(root, "metadata", "name"), "") == "" {
		ui.Errorf(l.ErrOut, "  Missing required field: metadata.name")
		errs++
	}

	if isFile(domainDir + "/cluster.lok8s.yaml") {
		// yq: `.spec.kind // .kind // ""`
		specRuntime := valueOr(nodeAt(root, "spec", "kind"), "")
		if specRuntime == "" {
			specRuntime = valueOr(nodeAt(root, "kind"), "")
		}
		if specRuntime == "" || specRuntime == "null" {
			ui.Warnf(l.ErrOut, "  Missing spec.kind (cluster runtime type)")
		}
	}
	return errs
}

// clusterref validates spec.clusterRef for deploy domains (bash:
// lint::clusterref). Returns the number of errors.
func (l *Linter) clusterref(domainDir, specFile string) int {
	if !isFile(domainDir + "/deploy.lok8s.yaml") {
		return 0
	}
	root := firstDoc(specFile)
	errs := 0

	clusterRef := valueOr(nodeAt(root, "spec", "clusterRef"), "")
	if clusterRef == "" || clusterRef == "null" {
		ui.Errorf(l.ErrOut, "  Missing required field: spec.clusterRef")
		errs++
		return errs
	}
	// Validate clusterRef.domain points to a valid cluster domain
	refDomain := valueOr(nodeAt(root, "spec", "clusterRef", "domain"), "")
	if refDomain != "" {
		if !isDir(l.Paths.Clusters + "/" + refDomain) {
			ui.Errorf(l.ErrOut, "  clusterRef.domain '%s' not found in .lok8s/", refDomain)
			errs++
		} else if !isFile(l.Paths.Clusters + "/" + refDomain + "/cluster.lok8s.yaml") {
			ui.Errorf(l.ErrOut, "  clusterRef.domain '%s' has no cluster.lok8s.yaml", refDomain)
			errs++
		}
	}
	return errs
}

// kustomization validates kustomization.yaml exists in targets and its
// resource references resolve (bash: lint::kustomization). Returns the
// number of errors.
func (l *Linter) kustomization(domainDir string) int {
	errs := 0
	targetsDir := domainDir + "/targets"
	if !isDir(targetsDir) {
		return 0
	}
	for _, tname := range sortedDirNames(targetsDir) {
		tdir := targetsDir + "/" + tname + "/"
		kustfile := tdir + "kustomization.yaml"
		if !isFile(kustfile) {
			ui.Warnf(l.ErrOut, "  Target %s/ missing kustomization.yaml", tname)
			continue
		}
		for _, item := range seqItems(nodeAt(firstDoc(kustfile), "resources")) {
			// yq -r renders a null element as the literal "null" (which then
			// fails the existence check, like bash); a map/seq element would
			// render as multi-line YAML — not a path, skipped here.
			res := scalarText(item)
			if res == "" {
				continue
			}
			// Remote references (URLs, repo refs) are not checked locally
			if strings.Contains(res, "://") {
				continue
			}
			// Resources may be files or directories (bases)
			if !exists(tdir + res) {
				ui.Errorf(l.ErrOut, "  Target %s/: kustomization.yaml references missing path: %s", tname, res)
				errs++
			}
		}
	}
	return errs
}

var lok8sLabelRe = regexp.MustCompile(`^lok8s\.dev/`)

// labels warns about manifests missing lok8s.dev/* labels (bash:
// lint::labels; warnings only).
func (l *Linter) labels(domainDir string) {
	targetsDir := domainDir + "/targets"
	if !isDir(targetsDir) {
		return
	}
	for _, tname := range sortedDirNames(targetsDir) {
		tdir := targetsDir + "/" + tname + "/"
		for _, mbase := range sortedFileNames(targetsDir+"/"+tname, ".yaml") {
			if mbase == "kustomization.yaml" {
				continue
			}
			if labelsQuery(tdir+mbase) == "0" {
				ui.Warnf(l.ErrOut, "  %s/%s: missing lok8s.dev/* label", tname, mbase)
			}
		}
	}
}

// labelsQuery replays the bash label probe byte for byte:
//
//	yq -r '.metadata.labels | keys | map(select(test("^lok8s\\.dev/"))) | length' 2>/dev/null || echo "0"
//
// yq emits one count PER DOCUMENT and aborts at the first document whose
// .metadata.labels is not a map (the `|| echo "0"` then appends a lone "0").
// The bash test fires only when the whole capture equals exactly "0" — so a
// multi-document manifest whose FIRST document carries the label never warns,
// while an unparseable file always does. Faithfully quirky.
func labelsQuery(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "0"
	}
	docs := parseDocs(raw)
	if docs == nil {
		return "0" // parse error → yq rc≠0, no output → echo "0"
	}
	var outputs []string
	for _, doc := range docs {
		labels := nodeAt(doc, "metadata", "labels")
		if keys, ok := mapKeys(labels); ok {
			count := 0
			for _, k := range keys {
				if lok8sLabelRe.MatchString(k) {
					count++
				}
			}
			outputs = append(outputs, strconv.Itoa(count))
			continue
		}
		// `keys` on a non-map errors: yq stops, echo appends "0".
		outputs = append(outputs, "0")
		break
	}
	return strings.Join(outputs, "\n")
}

var secretDataRe = regexp.MustCompile(`(?m)^(data|stringData):`)

// secrets warns about unencrypted secret files in the secrets/ directory and
// in the secrets store (bash: lint::secrets; warnings only — the underlying
// checks' non-zero returns are deliberately swallowed, exactly like the
// `|| true` the bash pipes need under `set -euo pipefail`).
func (l *Linter) secrets(domainDir, domain string) {
	// Legacy per-domain secrets/ directory (YAML Secret manifests)
	secretsDir := domainDir + "/secrets"
	if isDir(secretsDir) {
		for _, sbase := range sortedFileNames(secretsDir, "") {
			if strings.HasSuffix(sbase, ".enc") || strings.HasSuffix(sbase, ".age") {
				continue
			}
			raw, err := os.ReadFile(secretsDir + "/" + sbase)
			if err == nil && secretDataRe.Match(raw) {
				ui.Warnf(l.ErrOut, "  secrets/%s: appears unencrypted (contains data/stringData)", sbase)
			}
		}
	}

	// Secret cache ($PATH_SECRETS) — check for missing .enc counterparts.
	// The bash pipes check_unencrypted's stderr through `warn "  <line>"`, so
	// each captured line (already carrying its own colored [warn] prefix)
	// gains a second one. Preserved: the nested prefix IS the bash output.
	ctx := &secrets.Context{Paths: l.Paths, Domain: domain}
	var buf bytes.Buffer
	secrets.CheckUnencrypted(ctx.StorePath(), &buf)
	for _, line := range bufLines(&buf) {
		ui.Warnf(l.ErrOut, "  %s", line)
	}

	// Deprecated flat-store shadows: a per-domain secret ALSO present in the
	// flat .secrets/ store. The two diverge silently and different tools read
	// different stores, which can re-key a live cluster from the wrong one.
	// (check_flat_shadows echoes to stdout — no 2>&1 — so the single warn
	// below is the only prefix.)
	flat := l.Paths.SecretsEnv
	if flat == "" {
		flat = l.Paths.Base + "/.secrets"
	}
	buf.Reset()
	secrets.CheckFlatShadows(flat, domainDir, &buf)
	for _, line := range bufLines(&buf) {
		ui.Warnf(l.ErrOut, "  %s", line)
	}
}

// apex is the repo-global topology check: a cluster.lok8s.yaml must NOT be a
// subdomain of another cluster.lok8s.yaml. ONE cluster per plane/apex —
// subdomains (app., api., auth., kkp. …) are routing + targets INSIDE the
// apex cluster, not their own cluster specs. Catches the recurring mistake of
// inventing clusters/<sub>.<apex>/cluster.lok8s.yaml (see docs/guide/concepts:
// "One cluster per plane — NOT per subdomain"). deploy.lok8s.yaml domains are
// exempt (they are deploy targets with a clusterRef, not clusters). Returns
// false on any violation.
func (l *Linter) apex() bool {
	var domains []string
	for _, d := range sortedDirNames(l.Paths.Clusters) {
		if isFile(l.Paths.Clusters + "/" + d + "/cluster.lok8s.yaml") {
			domains = append(domains, d)
		}
	}

	ok := true
	for _, a := range domains {
		for _, b := range domains {
			if a == b {
				continue
			}
			// a is a strict subdomain of b (a ends with ".b")
			if strings.HasSuffix(a, "."+b) {
				ui.Errorf(l.ErrOut, "  cluster '%s' is a subdomain of cluster '%s' — one cluster per plane: express '%s' as a target/HTTPRoute inside clusters/%s/, not a separate cluster.lok8s.yaml (docs/guide/concepts: 'One cluster per plane')", a, b, a, b)
				ok = false
			}
		}
	}
	return ok
}

// bufLines splits a captured stream into its complete lines.
func bufLines(buf *bytes.Buffer) []string {
	s := strings.TrimRight(buf.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// isFile mirrors bash [[ -f ]] (follows symlinks).
func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// isDir mirrors bash [[ -d ]].
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// exists mirrors bash [[ -e ]].
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
