package addons

// guide_versions_test.go: the versions that the docs restate equal the addon
// pins. The guide names the pinned chart versions in the "Framework addons"
// table of docs/guide/addons.md, and in prose and example output elsewhere in
// docs/. Nothing else ties these numbers to the pins, so a chart bump would
// leave the docs stale while CI stays green.
//
//   - TestGuideAddonTableMatchesThePins: each row of the table against the
//     ChartRenderer documents of its addon, and the scope of the table.
//   - TestDocsRestatementsMatchThePins: each other restatement, one entry in
//     docsRestatements or docsImageRestatements.
//   - TestDocsHaveNoUncheckedPin: a scan of the Markdown pages of docs/ (not
//     the release notes) for each pinned version. Each mention must be one
//     that the two tests above check, or an entry of docsNotPins with its
//     reason. So a new restatement of a pin must join a table here.
//
// The scan looks for the pinned versions, not for chart names. A mention
// with a version that was never the pin (a typo, or history) cannot go stale
// on a bump, and the scan does not see it.
//
// The tests read the checkout (testutil.RepoRoot) and the embedded addons
// tree, as the other addon tests do (the tree-drift gate keeps it equal to
// .lok8s/addons). The release archives hold only the binary and the license,
// so docs/ is not there.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
	"gopkg.in/yaml.v3"
)

const (
	addonsTree     = "internal/assets/lok8s/addons"
	docsDir        = "docs"
	guideAddons    = "guide/addons.md" // relative to docs/, as every file name below
	guideTableHead = "## Framework addons"
	// guideTableRows is the number of rows of the table today. It is a
	// floor, so a new row needs no change here.
	guideTableRows = 12
	// guideVersion is a version as the docs write it: an optional v,
	// major.minor.patch, and an optional pre-release or build suffix.
	guideVersion = `v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]*[0-9A-Za-z])?`
)

// guideChartRefRE is one chart of the Chart column: `<alias>/<chart>` or
// `<chart>`, a space, then the version.
var guideChartRefRE = regexp.MustCompile("^`(?:([^`/]+)/)?([^`/]+)` (" + guideVersion + ")$")

// guideRepoAliases is the repository alias that the table writes before the
// chart, for each chart repository of a row. "" means that the table writes
// the chart name alone. A row with a chart from a repository that is not
// here fails, so a new repository needs its alias here.
var guideRepoAliases = map[string]string{
	"https://helm.cilium.io/":                         "cilium",
	"https://metallb.github.io/metallb":               "metallb",
	"https://charts.jetstack.io":                      "jetstack",
	"https://charts.hetzner.cloud":                    "",
	"oci://docker.io/envoyproxy":                      "envoyproxy",
	"https://kubernetes-sigs.github.io/external-dns/": "external-dns",
	"https://charts.rook.io/release":                  "rook-release",
	"https://ceph.github.io/ceph-csi-operator":        "",
	"https://ot-container-kit.github.io/helm-charts/": "ot-container-kit",
	"https://charts.rancher.io":                       "rancher",
}

// guideAddonsWithoutRow are the addons with a pinned chart that the table
// does not list: the guide says that the table lists a part of the addons.
// A new addon with a pinned chart needs a row or an entry here. An entry
// that has a row, or that pins no chart, fails.
var guideAddonsWithoutRow = []string{
	"autoscaler", "ccm", "cnpg-operator", "fluxcd", "gatus", "loki",
	"metrics-server", "monitoring", "promtail", "reflector", "tempo",
}

// docsRestatements are the chart pins that the docs restate outside the
// table. In re, {v} stands for the version as written. Each re must match
// once in its file. exact: the text is the output of a command, which prints
// the version as the ChartRenderer pins it, so the v is compared too. In
// prose, a leading v is not compared.
var docsRestatements = []struct {
	file, re, addon, chart string
	exact                  bool
}{
	{"guide/audit.md", `(?m)^cilium +networking +khelm +{v} `, "cilium", "cilium", true},
	{"guide/audit.md", `(?m)^ccm +networking +khelm +{v} `, "ccm", "hcloud-cloud-controller-manager", true},
	{"guide/audit.md", `(?m)^cert-manager +infrastructure +khelm +{v} `, "cert-manager", "cert-manager", true},
	{"guide/inventory.md", `(?m)^ +cilium {v} \(networking\)$`, "cilium", "cilium", true},
	{"guide/inventory.md", `(?m)^ +cert-manager {v} \(infrastructure\)$`, "cert-manager", "cert-manager", true},
	{"guide/inventory.md", `"name": "cilium", "chartVersion": "{v}"`, "cilium", "cilium", true},
	{"guide/inventory.md", `"name": "cert-manager", "chartVersion": "{v}"`, "cert-manager", "cert-manager", true},
	// The ceph-csi-operator that rook-ceph installs has the version of the
	// ceph-csi-drivers chart: the addon keeps the two in lockstep.
	{"guide/addons.md", `the plugin image of ceph-csi-operator {v}\)`, "rook-ceph", "ceph-csi-drivers", false},
	{"guide/addons.md", `Redis operator\n\(chart {v}, operator `, "redis-operator", "redis-operator", false},
	{"guide/addons.md", `installs ExternalDNS \(chart {v}\)`, "external-dns", "external-dns", false},
	{"reference/specs.md", "MetalLB is pinned to Helm chart version `{v}`", "metallb", "metallb", false},
}

// docsImageRestatements are the image tags that the docs restate, as
// `<repository>:<tag>@sha256:…`. The tag must equal the tag of the image at
// the key path in the addon's values.yaml, without its digest, and the
// repository must be the same.
var docsImageRestatements = []struct {
	file, repository, addon string
	values                  []string
}{
	{guideAddons, "rancher/system-upgrade-controller", "system-upgrade-controller", []string{"systemUpgradeController", "image"}},
	{guideAddons, "rancher/kuberlr-kubectl", "system-upgrade-controller", []string{"kubectl", "image"}},
}

// notPin is a mention of a pinned version that does not restate the pin.
// text is a part of the line that holds the mention. With section, text is
// a heading, and the entry covers each mention under it, up to the next
// heading of the same or a higher level.
type notPin struct {
	file, text, why string
	section         bool
}

// docsNotPins are the mentions in docs/ that do not restate the pin. Each
// entry must still cover a mention, so a stale entry fails.
var docsNotPins = []notPin{
	{guideAddons, "| v1.9.3 | v1.6.1 |", "a row of the release history of envoy-gateway", false},
	{guideAddons, "#### Upgrading to v1.9.3 {#envoy-gateway-v1-9-3-hazards}", "the upgrade notes of the envoy-gateway v1.9.3 release", true},
}

// chartPin is one ChartRenderer document of an addon: the chart (a name, or
// the path of a local chart), its version ("" for a local chart), its
// repository and its file, relative to the addons tree.
type chartPin struct{ chart, version, repository, file string }

// docsMention is a pin as the docs write it: the file (under docs/), the line
// and the version as written.
type docsMention struct {
	file    string
	line    int
	version string
}

// readChartPins reads the ChartRenderer documents of each addon directory
// under dir, keyed by the addon name; an addon without one has an empty
// entry. Each YAML file is parsed, except in a local chart (a directory with
// a Chart.yaml: its templates are not YAML). A file that does not parse
// fails the test.
func readChartPins(t *testing.T, dir string) map[string][]chartPin {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string][]chartPin{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		addon := e.Name()
		pins[addon] = []chartPin{}
		err := filepath.WalkDir(filepath.Join(dir, addon), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if _, err := os.Stat(filepath.Join(path, "Chart.yaml")); err == nil {
					return filepath.SkipDir
				}
				return nil
			}
			if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, path)
			dec := yaml.NewDecoder(bytes.NewReader(raw))
			for {
				var n yaml.Node
				if err := dec.Decode(&n); errors.Is(err, io.EOF) {
					return nil
				} else if err != nil {
					return fmt.Errorf("%s: %w", rel, err)
				}
				var kind struct {
					Kind string `yaml:"kind"`
				}
				if n.Decode(&kind) != nil || kind.Kind != "ChartRenderer" {
					continue // not a mapping with a string kind, or another kind
				}
				var doc struct {
					Chart      string `yaml:"chart"`
					Version    string `yaml:"version"`
					Repository string `yaml:"repository"`
				}
				if err := n.Decode(&doc); err != nil {
					return fmt.Errorf("%s: %w", rel, err)
				}
				pins[addon] = append(pins[addon], chartPin{doc.Chart, doc.Version, doc.Repository, filepath.ToSlash(rel)})
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return pins
}

// onePin is the ChartRenderer of chart in the addon. Not one (none, or two
// of the same chart) is a problem.
func onePin(pins map[string][]chartPin, addon, chart string) (chartPin, string) {
	var found []chartPin
	for _, p := range pins[addon] {
		if p.chart == chart {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		return chartPin{}, fmt.Sprintf("the addon %s has %d ChartRenderers of the chart %q, want 1: %+v", addon, len(found), chart, found)
	}
	return found[0], ""
}

// readDoc reads a file under docs/.
func readDoc(t *testing.T, root, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, docsDir, file))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// checkGuideTable checks the rows of the "Framework addons" table. It
// returns the chart versions that it checked, the addons with a row and a
// problem for each mismatch. A Chart cell is one of:
//
//   - "local chart": each ChartRenderer of the addon renders a chart from a
//     path of the addon ("./…"), with no version;
//   - "raw manifests": the addon has no ChartRenderer;
//   - a list of "`<alias>/<chart>` <version>", separated by ", ": each chart
//     has one ChartRenderer in the addon with that version, the alias is the
//     one of its repository in guideRepoAliases, and each versioned
//     ChartRenderer of the addon is in the list. The table writes a v before
//     some versions that the chart pins without one, so a leading v is not
//     compared.
//
// A table line after the end of the table (before the next heading) is a
// problem: a row that wraps onto two lines ends the table there.
func checkGuideTable(t *testing.T, root string, pins map[string][]chartPin) (checked []docsMention, rows []string, problems []string) {
	t.Helper()
	lines := strings.Split(readDoc(t, root, guideAddons), "\n")
	start := slices.Index(lines, guideTableHead)
	if start < 0 {
		t.Fatalf("%s/%s: no line %q", docsDir, guideAddons, guideTableHead)
	}
	state := 0 // 0: before the table, 1: in it, 2: after it
	for i := start + 1; i < len(lines) && !strings.HasPrefix(lines[i], "#"); i++ {
		line := lines[i]
		where := fmt.Sprintf("%s/%s:%d", docsDir, guideAddons, i+1)
		switch {
		case !strings.HasPrefix(line, "|"):
			if state == 1 {
				state = 2
			}
			continue
		case state == 2:
			problems = append(problems, fmt.Sprintf("%s: a table line after the end of the table (a row that wraps onto two lines ends the table)", where))
			continue
		}
		state = 1
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 3 {
			problems = append(problems, fmt.Sprintf("%s: %d cells, want 3 (Addon, What it installs, Chart)", where, len(cells)))
			continue
		}
		addon := strings.Trim(strings.TrimSpace(cells[0]), "`")
		cell := strings.TrimSpace(cells[2])
		if addon == "Addon" || strings.HasPrefix(addon, "---") {
			continue // the header and the separator
		}
		rows = append(rows, addon)
		addonPins, ok := pins[addon]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: the addon %q is not in %s", where, addon, addonsTree))
			continue
		}
		switch cell {
		case "local chart":
			if len(addonPins) == 0 || slices.ContainsFunc(addonPins, func(p chartPin) bool {
				return p.version != "" || !strings.HasPrefix(p.chart, "./")
			}) {
				problems = append(problems, fmt.Sprintf("%s: %s says \"local chart\", but its ChartRenderers are %+v", where, addon, addonPins))
			}
		case "raw manifests":
			if len(addonPins) > 0 {
				problems = append(problems, fmt.Sprintf("%s: %s says \"raw manifests\", but it has the ChartRenderers %+v", where, addon, addonPins))
			}
		default:
			named := map[string]bool{}
			for item := range strings.SplitSeq(cell, ", ") {
				m := guideChartRefRE.FindStringSubmatch(item)
				if m == nil {
					problems = append(problems, fmt.Sprintf("%s: the chart %q is not \"`<alias>/<chart>` <version>\", \"local chart\" or \"raw manifests\"", where, item))
					continue
				}
				alias, chart, version := m[1], m[2], m[3]
				named[chart] = true
				pin, problem := onePin(pins, addon, chart)
				if problem != "" {
					problems = append(problems, where+": "+problem)
					continue
				}
				checked = append(checked, docsMention{guideAddons, i + 1, version})
				if want, ok := guideRepoAliases[pin.repository]; !ok {
					problems = append(problems, fmt.Sprintf("%s: the repository %s of %s has no entry in guideRepoAliases", where, pin.repository, chart))
				} else if alias != want {
					problems = append(problems, fmt.Sprintf("%s: %s has the alias %q, want %q (the repository %s)", where, chart, alias, want, pin.repository))
				}
				if strings.TrimPrefix(version, "v") != strings.TrimPrefix(pin.version, "v") {
					problems = append(problems, fmt.Sprintf("%s: %s %s, but %s/%s pins %s", where, chart, version, addonsTree, pin.file, pin.version))
				}
			}
			for _, p := range addonPins {
				if p.version != "" && !named[p.chart] {
					problems = append(problems, fmt.Sprintf("%s: the row of %s does not name the chart %s %s (%s/%s)", where, addon, p.chart, p.version, addonsTree, p.file))
				}
			}
		}
	}
	return checked, rows, problems
}

// findOnce finds re in text. With exactly one match, it returns its first
// group and the 1-based line of the group.
func findOnce(re *regexp.Regexp, text string) (group string, line, matches int) {
	all := re.FindAllStringSubmatchIndex(text, -1)
	if len(all) != 1 {
		return "", 0, len(all)
	}
	m := all[0]
	return text[m[2]:m[3]], 1 + strings.Count(text[:m[2]], "\n"), 1
}

// checkDocsRestatements checks docsRestatements and docsImageRestatements.
// It returns the versions that it checked and a problem for each mismatch.
func checkDocsRestatements(t *testing.T, root string, pins map[string][]chartPin) (checked []docsMention, problems []string) {
	t.Helper()
	for _, r := range docsRestatements {
		where := docsDir + "/" + r.file
		re := regexp.MustCompile(strings.Replace(r.re, "{v}", "("+guideVersion+")", 1))
		got, line, n := findOnce(re, readDoc(t, root, r.file))
		if n != 1 {
			problems = append(problems, fmt.Sprintf("%s: %d matches of %s, want 1: the restatement moved or was reworded, so update docsRestatements", where, n, r.re))
			continue
		}
		pin, problem := onePin(pins, r.addon, r.chart)
		if problem != "" {
			problems = append(problems, fmt.Sprintf("%s:%d: %s", where, line, problem))
			continue
		}
		checked = append(checked, docsMention{r.file, line, got})
		want := pin.version
		if !r.exact {
			got, want = strings.TrimPrefix(got, "v"), strings.TrimPrefix(want, "v")
		}
		if got != want {
			problems = append(problems, fmt.Sprintf("%s:%d: %s %s, but %s/%s pins %s", where, line, r.chart, got, addonsTree, pin.file, pin.version))
		}
	}
	for _, r := range docsImageRestatements {
		where := docsDir + "/" + r.file
		re := regexp.MustCompile("`" + regexp.QuoteMeta(r.repository) + ":([^@`]+)@sha256:")
		got, line, n := findOnce(re, readDoc(t, root, r.file))
		if n != 1 {
			problems = append(problems, fmt.Sprintf("%s: %d matches of %s, want 1: the restatement moved or was reworded, so update docsImageRestatements", where, n, re))
			continue
		}
		repository, tag := valuesImage(t, filepath.Join(root, addonsTree, r.addon, "values.yaml"), r.values)
		tag, _, _ = strings.Cut(tag, "@")
		checked = append(checked, docsMention{r.file, line, got})
		if repository != r.repository || got != tag {
			problems = append(problems, fmt.Sprintf("%s:%d: %s:%s, but %s/%s/values.yaml %s pins %s:%s", where, line, r.repository, got, addonsTree, r.addon, strings.Join(r.values, "."), repository, tag))
		}
	}
	return checked, problems
}

// valuesImage reads the repository and the tag of the image at the key path
// of a values file.
func valuesImage(t *testing.T, path string, keys []string) (repository, tag string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var node any
	if err := yaml.Unmarshal(raw, &node); err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		m, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("%s: no key path %s", path, strings.Join(keys, "."))
		}
		node = m[k]
	}
	image, _ := node.(map[string]any)
	repository, _ = image["repository"].(string)
	tag, _ = image["tag"].(string)
	if repository == "" || tag == "" {
		t.Fatalf("%s: %s has no repository or tag", path, strings.Join(keys, "."))
	}
	return repository, tag
}

// versionTokenRE finds a version-like token: an optional v, then numbers
// with dots. The longest token wins, so 1.20.45 is not a mention of 1.20.4.
var versionTokenRE = regexp.MustCompile(`v?[0-9]+(?:\.[0-9]+)+`)

// scanVersions finds each mention of a version of want (keyed without a
// leading v) in the Markdown files under dir, except in the release notes
// and in build output. A mention stands alone: no letter, digit or dot
// before it, and no letter, digit or underscore after it.
func scanVersions(t *testing.T, dir string, want map[string]bool) []docsMention {
	t.Helper()
	var hits []docsMention
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "releases", "dist", "cache":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		for i, line := range strings.Split(string(raw), "\n") {
			for _, m := range versionTokenRE.FindAllStringIndex(line, -1) {
				before, after := byte(' '), byte(' ')
				if m[0] > 0 {
					before = line[m[0]-1]
				}
				if m[1] < len(line) {
					after = line[m[1]]
				}
				if isAlnum(before) || before == '.' || isAlnum(after) || after == '_' {
					continue
				}
				if v := line[m[0]:m[1]]; want[strings.TrimPrefix(v, "v")] {
					hits = append(hits, docsMention{filepath.ToSlash(rel), i + 1, v})
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func isAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// uncheckedMentions returns each hit that no checked mention and no entry of
// notPins covers, and each entry that covers no hit. A checked mention covers
// a hit on its line with the same version (a leading v aside). lines returns
// the lines of a file under docs/.
func uncheckedMentions(hits, checked []docsMention, notPins []notPin, lines func(file string) []string) (unchecked []docsMention, stale []string) {
	same := func(a, b docsMention) bool {
		return a.file == b.file && a.line == b.line && strings.TrimPrefix(a.version, "v") == strings.TrimPrefix(b.version, "v")
	}
	used := make([]bool, len(notPins))
	for _, h := range hits {
		if slices.ContainsFunc(checked, func(c docsMention) bool { return same(c, h) }) {
			continue
		}
		covered := false
		for i, np := range notPins {
			if np.file == h.file && notPinCovers(lines(h.file), np.text, np.section, h.line) {
				used[i], covered = true, true
			}
		}
		if !covered {
			unchecked = append(unchecked, h)
		}
	}
	for i, np := range notPins {
		if !used[i] {
			stale = append(stale, np.file+": "+np.text)
		}
	}
	return unchecked, stale
}

// notPinCovers reports whether an entry of docsNotPins covers the 1-based
// line: the line holds text, or, with section, the line is the heading text
// or under it, before the next heading of the same or a higher level. A #
// line in a code fence is not a heading.
func notPinCovers(lines []string, text string, section bool, line int) bool {
	if !section {
		return strings.Contains(lines[line-1], text)
	}
	head := slices.Index(lines, text)
	if head < 0 || line-1 < head {
		return false
	}
	level := len(text) - len(strings.TrimLeft(text, "#"))
	fence := false
	for _, l := range lines[head+1 : line] {
		if strings.HasPrefix(l, "```") {
			fence = !fence
		}
		if n := len(l) - len(strings.TrimLeft(l, "#")); !fence && n > 0 && n <= level && strings.HasPrefix(l[n:], " ") {
			return false
		}
	}
	return true
}

func TestGuideAddonTableMatchesThePins(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	pins := readChartPins(t, filepath.Join(root, addonsTree))
	checked, rows, problems := checkGuideTable(t, root, pins)
	for _, p := range problems {
		t.Error(p)
	}
	if len(rows) < guideTableRows || len(checked) == 0 {
		t.Errorf("%s/%s: %d rows and %d chart versions in the %q table, want at least %d rows. A row was removed, or the parser does not see it. "+
			"If a row was removed on purpose, lower guideTableRows", docsDir, guideAddons, len(rows), len(checked), guideTableHead, guideTableRows)
	}
	// The scope: each addon with a pinned chart has a row or an entry in
	// guideAddonsWithoutRow, not both.
	for addon, ps := range pins {
		versioned := slices.ContainsFunc(ps, func(p chartPin) bool { return p.version != "" })
		row, listed := slices.Contains(rows, addon), slices.Contains(guideAddonsWithoutRow, addon)
		switch {
		case versioned && !row && !listed:
			t.Errorf("%s pins a chart and has no row in the %q table: add a row, or add it to guideAddonsWithoutRow", addon, guideTableHead)
		case listed && (row || !versioned):
			t.Errorf("guideAddonsWithoutRow: %s has a row or pins no chart: remove the entry", addon)
		}
	}
	for _, addon := range guideAddonsWithoutRow {
		if _, ok := pins[addon]; !ok {
			t.Errorf("guideAddonsWithoutRow: %s is not in %s: remove the entry", addon, addonsTree)
		}
	}
}

func TestDocsRestatementsMatchThePins(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	_, problems := checkDocsRestatements(t, root, readChartPins(t, filepath.Join(root, addonsTree)))
	for _, p := range problems {
		t.Error(p)
	}
}

func TestDocsHaveNoUncheckedPin(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	pins := readChartPins(t, filepath.Join(root, addonsTree))
	table, _, _ := checkGuideTable(t, root, pins)
	restated, _ := checkDocsRestatements(t, root, pins)
	want := map[string]bool{}
	for _, ps := range pins {
		for _, p := range ps {
			if p.version != "" {
				want[strings.TrimPrefix(p.version, "v")] = true
			}
		}
	}
	for _, r := range docsImageRestatements {
		_, tag := valuesImage(t, filepath.Join(root, addonsTree, r.addon, "values.yaml"), r.values)
		tag, _, _ = strings.Cut(tag, "@")
		want[strings.TrimPrefix(tag, "v")] = true
	}
	hits := scanVersions(t, filepath.Join(root, docsDir), want)
	checked := slices.Concat(table, restated)
	// Each checked mention is a hit too: a checked mention that the scan
	// misses means that the scan is broken.
	for _, c := range checked {
		if !slices.Contains(hits, c) {
			t.Errorf("%s/%s:%d: the scan does not find the checked version %s, so scanVersions is broken", docsDir, c.file, c.line, c.version)
		}
	}
	files := map[string][]string{}
	lines := func(file string) []string {
		if _, ok := files[file]; !ok {
			files[file] = strings.Split(readDoc(t, root, file), "\n")
		}
		return files[file]
	}
	unchecked, stale := uncheckedMentions(hits, checked, docsNotPins, lines)
	for _, h := range unchecked {
		t.Errorf("%s/%s:%d: %s is a pinned version, and no test checks this mention. "+
			"If it restates the pin, add it to docsRestatements (or docsImageRestatements). If not, add it to docsNotPins with the reason",
			docsDir, h.file, h.line, h.version)
	}
	for _, s := range stale {
		t.Errorf("docsNotPins: %s covers no mention any more: remove the entry", s)
	}
}

// The scan and the coverage on a fixture: the bounds of a mention, the @ and
// : forms, the files that the scan skips, a checked mention, a line entry, a
// section entry that ends at the next heading of its level (a # line in a
// code fence does not end it), and a stale entry.
func TestDocsVersionScanFixture(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	text := "Pinned: cilium 1.20.4.\n" +
		"Not: go1.20.4 x1.20.4 x.1.20.4 11.20.4 1.20.45 1.20.4.1 1.20.4a 1.20.4_x\n" +
		"rook @v1.20.4 and image:v1.20.4\n" +
		"| v1.20.4 | old |\n" +
		"## Notes on 1.20.4\n" +
		"### Detail\n" +
		"```sh\n" +
		"# a comment, not a heading\n" +
		"```\n" +
		"Still 1.20.4 here.\n" +
		"## Next\n" +
		"New 1.20.4 here.\n"
	testutil.WriteFile(t, filepath.Join(dir, "guide", "a.md"), text)
	testutil.WriteFile(t, filepath.Join(dir, "guide", "a.txt"), "1.20.4\n")
	testutil.WriteFile(t, filepath.Join(dir, "releases", "v1.md"), "1.20.4\n")
	hits := scanVersions(t, dir, map[string]bool{"1.20.4": true})
	var got []string
	for _, h := range hits {
		got = append(got, fmt.Sprintf("%s:%d:%s", h.file, h.line, h.version))
	}
	want := []string{"guide/a.md:1:1.20.4", "guide/a.md:3:v1.20.4", "guide/a.md:3:v1.20.4", "guide/a.md:4:v1.20.4",
		"guide/a.md:5:1.20.4", "guide/a.md:10:1.20.4", "guide/a.md:12:1.20.4"}
	if !slices.Equal(got, want) {
		t.Fatalf("hits: got %q, want %q", got, want)
	}

	lines := func(string) []string { return strings.Split(text, "\n") }
	checked := []docsMention{{"guide/a.md", 1, "1.20.4"}, {"guide/a.md", 3, "1.20.4"}}
	notPins := []notPin{
		{"guide/a.md", "| v1.20.4 | old |", "history", false},
		{"guide/a.md", "## Notes on 1.20.4", "notes", true},
		{"guide/a.md", "metallb 0.1.0", "gone", false},
	}
	unchecked, stale := uncheckedMentions(hits, checked, notPins, lines)
	if len(unchecked) != 1 || unchecked[0].line != 12 {
		t.Errorf("unchecked: got %+v, want only line 12", unchecked)
	}
	if !slices.Equal(stale, []string{"guide/a.md: metallb 0.1.0"}) {
		t.Errorf("stale: got %q, want the metallb entry", stale)
	}
}
