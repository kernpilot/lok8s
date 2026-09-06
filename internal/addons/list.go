package addons

// list.go — the CLI half of .lok8s/libs/addons (`lo addons`): list, show
// and inventory (--detail) framework-shipped bootstrap addons. Read-only,
// cluster-free. Output is byte-identical to the bash implementation.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrHandled marks an error whose message was already printed in the bash
// format ([error] … on stderr).
var ErrHandled = ui.ErrHandled // one sentinel for every package; see internal/ui

// Dir is the project's addons directory (bash: addons::_dir). Since the
// eject model it is only half the picture: a framework addon the project
// holds no copy of is served from the binary (internal/assets) and listed
// with origin "builtin"; `lo addons <name>` ejects it there on first use.
func Dir(p *config.Paths) string { return p.Lok8s + "/addons" }

// Names is the union of the embedded addon names and the project's own
// addon dirs, in bash `*/` glob order.
func Names(p *config.Paths) []string {
	seen := map[string]bool{}
	var names []string
	for _, n := range assets.AddonNames() {
		seen[n] = true
		names = append(names, n)
	}
	for _, dir := range addonDirs(Dir(p)) {
		if n := filepath.Base(dir); !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Slice(names, func(i, j int) bool { return names[i]+"/" < names[j]+"/" })
	return names
}

// OriginOf is the origin column for one addon: builtin · local · local
// (modified) · local-only (assets.Report's unit verdict).
func OriginOf(p *config.Paths, name string) string {
	reports, err := assets.Report(p, []string{"addons/" + name})
	if err != nil || len(reports) == 0 {
		return "-"
	}
	return reports[0].Origin
}

// Driver resolves the driver kind for a domain (bash: addons::_driver).
// Falls back to "lo" when no spec is found — but NOT when the spec declares
// a malformed one: rc 1 ("nothing to read") takes the fallback, rc 2
// ("present but not a bare driver name") is never defaulted, because the
// value reaches a path we source and quietly replacing a crafted one with
// "lo" hides the crafting.
func Driver(p *config.Paths, target string, stderr io.Writer) (string, error) {
	spec := p.Clusters + "/" + target + "/cluster.lok8s.yaml"
	kind, err := domain.SpecDriver(spec, "lo")
	if err != nil {
		ui.Errorf(stderr, "cluster spec for '%s' declares a malformed kind (%s) — refusing to assume 'lo'", target, spec)
		return "", ErrHandled
	}
	return kind, nil
}

// chartMeta reads chart.yaml's kind/version/chart/repository the way the
// bash `yq -r '.X // "-"'` reads did: absent/null → "-", anything else its
// raw scalar text. An unreadable chart.yaml reads as all-absent.
type chartMeta struct {
	present                          bool
	kind, version, chart, repository string
}

func readChart(dir string) chartMeta {
	m := chartMeta{version: "-", chart: "-", repository: "-"}
	raw, err := os.ReadFile(filepath.Join(dir, "chart.yaml"))
	if err != nil {
		return m
	}
	m.present = true
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return m
	}
	doc := derefNode(&root)
	if doc == nil || doc.Kind != yaml.MappingNode {
		return m
	}
	get := func(key, fallback string) string {
		for i := 0; i+1 < len(doc.Content); i += 2 {
			if doc.Content[i].Value == key {
				v := derefNode(doc.Content[i+1])
				if v == nil || v.Kind != yaml.ScalarNode || v.Tag == "!!null" {
					return fallback
				}
				return v.Value
			}
		}
		return fallback
	}
	m.kind = get("kind", "")
	m.version = get("version", "-")
	m.chart = get("chart", "-")
	m.repository = get("repository", "-")
	return m
}

// Type classifies an addon directory: khelm | raw | empty (bash:
// addons::_type).
func Type(dir string) string {
	if m := readChart(dir); m.present && m.kind == "ChartRenderer" {
		return "khelm"
	}
	if fileExists(filepath.Join(dir, "kustomization.yaml")) || fileExists(filepath.Join(dir, "kustomization.yml")) {
		// Has kustomization but no chart.yaml → raw or composition
		return "raw"
	}
	return "empty"
}

// Version is the khelm addon's pinned chart version, "-" otherwise (bash:
// addons::_version).
func Version(dir string) string { return readChart(dir).version }

// addonDirs lists the addon directories under root in glob order (bash:
// for dir in "${addons_dir}"/*/ — dotdirs skipped, sorted bytewise).
func addonDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(root, e.Name())
		if info, err := os.Stat(full); err == nil && info.IsDir() {
			dirs = append(dirs, full)
		}
	}
	sortGlobDirs(dirs)
	return dirs
}

// sortGlobDirs orders directories the way a bash `*/` glob expands them
// under LC_ALL=C: the trailing slash is part of the compared string, so
// "cert-manager-webhook-hetzner/" sorts BEFORE "cert-manager/" ('-' < '/').
func sortGlobDirs(dirs []string) {
	sort.Slice(dirs, func(i, j int) bool { return dirs[i]+"/" < dirs[j]+"/" })
}

// List prints the table of framework addons (bash: addons::list): the
// union of the embedded set and the project's own dirs. The table is
// byte-identical to the bash output; ListOrigin adds the ORIGIN column.
func List(p *config.Paths, d string, out, stderr io.Writer) error {
	return list(p, d, out, stderr, false)
}

// ListOrigin is List with the origin column (`lo addons --origin`).
func ListOrigin(p *config.Paths, d string, out, stderr io.Writer) error {
	return list(p, d, out, stderr, true)
}

func list(p *config.Paths, d string, out, stderr io.Writer, withOrigin bool) error {
	if _, err := Driver(p, d, stderr); err != nil {
		return err
	}
	names := Names(p)
	if len(names) == 0 {
		ui.Warnf(stderr, "No addons directory (%s)", Dir(p))
		return nil
	}
	row := func(name, typ, version, origin, detail string) {
		if withOrigin {
			fmt.Fprintf(out, "%-20s  %-8s  %-12s  %-18s  %s\n", name, typ, version, origin, detail)
			return
		}
		fmt.Fprintf(out, "%-20s  %-8s  %-12s  %s\n", name, typ, version, detail)
	}
	row("NAME", "TYPE", "VERSION", "ORIGIN", "CHART/REPO")
	row("----", "----", "-------", "------", "----------")
	for _, name := range names {
		// Peek: a listing never ejects.
		dir, _, err := assets.Peek(p, "addons/"+name)
		if err != nil {
			continue
		}
		m := readChart(dir)
		detail := "-"
		if m.chart != "-" {
			detail = m.chart + " (" + m.repository + ")"
		}
		origin := ""
		if withOrigin {
			origin = OriginOf(p, name)
		}
		row(name, Type(dir), m.version, origin, detail)
	}
	return nil
}

// Show prints the detail of one addon (bash: addons::show). A framework
// addon the project holds no copy of is ejected first (default policy), so
// `path:` names the project's copy; ShowOrigin adds an `origin:` line.
func Show(p *config.Paths, d, name string, out, stderr io.Writer) error {
	return show(p, d, name, out, stderr, false)
}

// ShowOrigin is Show with the origin line (`lo addons <name> --origin`).
func ShowOrigin(p *config.Paths, d, name string, out, stderr io.Writer) error {
	return show(p, d, name, out, stderr, true)
}

func show(p *config.Paths, d, name string, out, stderr io.Writer, withOrigin bool) error {
	if name == "" {
		ui.Errorf(stderr, "addon name required")
		return ErrHandled
	}
	driver, err := Driver(p, d, stderr)
	if err != nil {
		return err
	}
	dir, origin, err := assets.Resolve(p, "addons/"+name)
	if err != nil || origin == assets.OriginNone {
		dir = Dir(p) + "/" + name
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		ui.Errorf(stderr, "addon '%s' not found (%s)", name, dir)
		return ErrHandled
	}
	fmt.Fprintf(out, "name:    %s\n", name)
	fmt.Fprintf(out, "driver:  %s\n", driver)
	fmt.Fprintf(out, "path:    %s\n", dir)
	if withOrigin {
		fmt.Fprintf(out, "origin:  %s\n", OriginOf(p, name))
	}
	fmt.Fprintf(out, "type:    %s\n", Type(dir))
	m := readChart(dir)
	if m.chart != "-" {
		fmt.Fprintf(out, "chart:   %s\n", m.chart)
		fmt.Fprintf(out, "version: %s\n", m.version)
		fmt.Fprintf(out, "repo:    %s\n", m.repository)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "files:")
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if fileExists(filepath.Join(dir, e.Name())) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(out, "  - %s\n", n)
	}
	return nil
}

var categoryRe = regexp.MustCompile(`lok8s\.dev/category:[[:space:]]*[a-zA-Z0-9-]+`)

// Category reads an addon's lok8s.dev/category label from its manifests
// (bash: addons::_category — `grep -rhoE … | LC_ALL=C sort | head -1`): every
// match across every regular file under dir, the bytewise-first winner, so
// the category feeds deterministic artifacts. "-" when unlabelled.
func Category(dir string) string {
	var matches []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		matches = append(matches, categoryRe.FindAllString(string(raw), -1)...)
		return nil
	})
	if len(matches) == 0 {
		return "-"
	}
	sort.Strings(matches)
	line := matches[0]
	cat := line[strings.LastIndex(line, ":")+1:]
	cat = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' {
			return -1
		}
		return r
	}, cat)
	if cat == "" {
		return "-"
	}
	return cat
}

// configHints is the small in-repo config-help catalog (bash:
// addons::_config_hint): every addon under .lok8s/addons/ MUST have an
// entry here (TestEveryAddonHasConfigHint fails on drift, so adding an
// addon without a hint breaks CI). Keep each hint to the handful of knobs
// users actually touch.
var configHints = map[string]string{
	"autoscaler":                   "min/max nodes per MachineDeployment via cluster-autoscaler annotations",
	"ccm":                          "spec.bootstrap ccm.values.env: ROBOT_ENABLED / HCLOUD_NETWORK (hcloud CCM)",
	"cert-manager":                 "issue TLS via ClusterIssuer/Certificate CRs in a networking target",
	"cert-manager-webhook-hetzner": "needs the hetzner-dns token Secret; pair with a DNS-01 ClusterIssuer",
	"cilium":                       "encryption + policy mode (policyAuditMode) in cilium inline values / values.<driver>.yaml",
	"cnpg-operator":                "declare databases as CNPG Cluster CRs in a target",
	"envoy-gateway":                "define Gateway + HTTPRoutes in a networking target",
	"external-dns":                 "set the managed DNS zones via spec.dns.domainFilter",
	"fluxcd":                       "GitRepository + Kustomization are per-cluster glue; set them inline (extraObjects) or in a flux-system target",
	"gatus":                        "configure monitored endpoints in the gatus target/values",
	"local-path-provisioner":       "set as default StorageClass; values in addons/local-path-provisioner",
	"loki":                         "bootstrap AFTER monitoring; pin storageClass/retention inline; add the Grafana Loki datasource in the monitoring values",
	"mailpit":                      "expose via an HTTPRoute target (dev SMTP capture)",
	"metallb":                      "set the address pool via spec.loadBalancer.pool",
	"metrics-server":               "usually no config; values in addons/metrics-server",
	"monitoring":                   "Prometheus/Grafana scrape + OIDC config via the monitoring target values",
	"oidc-rbac":                    "spec.oidc.groupsClaim + the oidc group->cluster-role bindings",
	"promtail":                     "bootstrap AFTER loki (its push target); tolerates all taints so control-plane logs ship too",
	"redis-operator":               "declare Redis CRs in a target",
	"reflector":                    "annotate the source Secret/ConfigMap to mirror across namespaces",
	"rook-ceph":                    "CephCluster/pool/StorageClass in a target: replica count + device filter",
	"sso-gate":                     "patch issuer/clientID from a target + Secret sso-gate-client; label routes sso.lok8s.dev/protect",
	"system-upgrade-controller":    "declare upgrade Plans in a target (Rancher SUC)",
	"tempo":                        "configure trace storage/retention via the tempo values",
}

// ConfigHint is the one-line "how to configure" pointer for a framework
// addon, "" when uncurated.
func ConfigHint(name string) string { return configHints[name] }

// Resolved is one spec.bootstrap entry as the bootstrap parser resolved it
// (the Name/Dir pair of bootstrap.Entry).
type Resolved struct {
	Name string
	Dir  string
	// Builtin marks a framework addon (bootstrap.Entry.Builtin) as opposed
	// to a per-cluster target.
	Builtin bool
}

// EntryResolver reads a cluster spec's bootstrap entries for a driver kind
// (bash: bootstrap::_resolve_entries + _parse_entry, unparsable entries
// skipped). The cli wires bootstrap.ResolveEntries/ParseEntry here — this
// package cannot import bootstrap (it imports the render half of addons).
type EntryResolver func(spec, kind, domain string) []Resolved

// Detail inventories the addons THIS cluster DEPLOYS (bash: addons::detail).
// Reads spec.bootstrap through the SAME shared parser the apply path uses
// (so map-form entries stay intact), intersected with the .lok8s/addons/
// tree, and prints each addon's category + a one-line config pointer.
// `./targets/*` (and absolute-path) entries are listed as per-cluster
// targets (glue), not framework addons.
func Detail(p *config.Paths, d string, out, stderr io.Writer, resolve EntryResolver) error {
	return detail(p, d, out, stderr, resolve, false)
}

// DetailOrigin is Detail with the origin column (`lo addons --detail
// --origin`).
func DetailOrigin(p *config.Paths, d string, out, stderr io.Writer, resolve EntryResolver) error {
	return detail(p, d, out, stderr, resolve, true)
}

func detail(p *config.Paths, d string, out, stderr io.Writer, resolve EntryResolver, withOrigin bool) error {
	// Reject a path-traversal / injected domain BEFORE it builds any
	// filesystem path (same guard as bootstrap::dispatch /
	// provision::resolve_spec / audit::run_domain).
	if !domain.NameRe.MatchString(d) {
		ui.Warnf(stderr, "Invalid domain name '%s' — nothing to inventory", d)
		return nil
	}
	spec := p.Clusters + "/" + d + "/cluster.lok8s.yaml"
	if !fileExists(spec) {
		ui.Warnf(stderr, "No cluster spec for '%s' (%s) — nothing to inventory", d, spec)
		return nil
	}
	// A malformed kind is reported, never guessed away: it selects the
	// values.<driver>.yaml overlay and the driver's default entries.
	kind, err := domain.SpecDriver(spec, "lo")
	if err != nil {
		ui.Errorf(stderr, "cluster spec for '%s' declares a malformed kind (%s)", d, spec)
		return ErrHandled
	}
	entries := resolve(spec, kind, d)

	fmt.Fprintf(out, "Addons deployed by %s (kind=%s)\n\n", d, kind)
	row := func(name, category, typ, version, origin, configure string) {
		if withOrigin {
			fmt.Fprintf(out, "%-24s  %-14s  %-8s  %-10s  %-18s  %s\n", name, category, typ, version, origin, configure)
			return
		}
		fmt.Fprintf(out, "%-24s  %-14s  %-8s  %-10s  %s\n", name, category, typ, version, configure)
	}
	row("NAME", "CATEGORY", "TYPE", "VERSION", "ORIGIN", "CONFIGURE")
	row("----", "--------", "----", "-------", "------", "---------")

	count := 0
	for _, e := range entries {
		count++
		if e.Builtin {
			hint := ConfigHint(filepath.Base(e.Dir))
			if hint == "" {
				hint = "see docs/guide/addons.md"
			}
			origin := ""
			if withOrigin {
				origin = OriginOf(p, filepath.Base(e.Dir))
			}
			row(e.Name, Category(e.Dir), Type(e.Dir), Version(e.Dir), origin, hint)
			continue
		}
		// Per-cluster target (glue) — a ./targets/* or /abs path entry.
		tpath := strings.ReplaceAll(e.Dir, "/./", "/")
		tpath = strings.TrimPrefix(tpath, p.Base+"/")
		origin := ""
		if withOrigin {
			origin = "target"
		}
		row(e.Name, "target", "target", "-", origin, "per-cluster glue in "+tpath)
	}
	if count == 0 {
		fmt.Fprintln(out, "  (spec.bootstrap is empty — this cluster deploys no addons)")
	}
	fmt.Fprint(out, "\nConfigure an addon: edit its spec.bootstrap inline values, or its addons/<name>/values.<driver>.yaml — see docs/guide/addons.md.\n")
	return nil
}
