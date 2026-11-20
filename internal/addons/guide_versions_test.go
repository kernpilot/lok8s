package addons

// guide_versions_test.go: the versions that the guide restates equal the
// addon pins. The guide names the pinned chart versions in the "Framework
// addons" table of docs/guide/addons.md, and in the example output of
// docs/guide/audit.md and docs/guide/inventory.md. Nothing else ties these
// numbers to the pins, so a chart bump would leave the guide stale while CI
// stays green.
//
//   - TestGuideAddonTableMatchesThePins: each row of the table against the
//     ChartRenderer documents of its addon. A pin can be in chart.yaml or in
//     another file of the addon (webhook.yaml, csi-drivers.yaml).
//   - TestGuideRestatementsMatchThePins: the other restatements, one entry
//     each in guideRestatements and guideImageRestatements.
//   - TestGuideNamesNoUncheckedPin: a scan of docs/guide for a pinned chart
//     or addon name followed by a version. Each mention must be one that the
//     two tests above check, or one in guideNotPins. So a new restatement
//     must join a table here.
//
// The tests read the checkout (testutil.RepoRoot). The release archives hold
// only the binary and the license, so docs/ is not there, and these tests
// run only in the repository.

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
	"sort"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
	"gopkg.in/yaml.v3"
)

const (
	guideDir        = "docs/guide"
	guideAddonsFile = "addons.md"
	guideTableHead  = "## Framework addons"
	// guideTableRows is the number of rows of the table today. It is a
	// floor, so a new row needs no change here. A table with fewer rows
	// fails: a row was removed, or the parser does not see it.
	guideTableRows = 12
	// guideVersion is a version as the guide writes it: an optional v,
	// major.minor.patch, and an optional pre-release or build suffix.
	guideVersion = `v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]*[0-9A-Za-z])?`
)

// guideChartRefRE is one chart of the Chart column: `<repo>/<chart>` or
// `<chart>`, a space, then the version.
var guideChartRefRE = regexp.MustCompile("^`([^`]+)` (" + guideVersion + ")$")

// guideRestatements are the chart pins that the guide restates outside the
// table, in the output of a command. re finds the restatement in file (under
// docs/guide), and its first group is the version as written. Each re must
// match once. The version must equal the pin exactly: the commands print the
// version as the ChartRenderer pins it, with or without its v.
var guideRestatements = []struct {
	file, re, addon, chart string
}{
	{"audit.md", `(?m)^cilium +networking +khelm +(\S+) `, "cilium", "cilium"},
	{"audit.md", `(?m)^ccm +networking +khelm +(\S+) `, "ccm", "hcloud-cloud-controller-manager"},
	{"audit.md", `(?m)^cert-manager +infrastructure +khelm +(\S+) `, "cert-manager", "cert-manager"},
	{"inventory.md", `(?m)^ +cilium (\S+) \(networking\)$`, "cilium", "cilium"},
	{"inventory.md", `(?m)^ +cert-manager (\S+) \(infrastructure\)$`, "cert-manager", "cert-manager"},
	{"inventory.md", `"name": "cilium", "chartVersion": "([^"]+)"`, "cilium", "cilium"},
	{"inventory.md", `"name": "cert-manager", "chartVersion": "([^"]+)"`, "cert-manager", "cert-manager"},
}

// guideImageRestatements are the image tags that the guide restates, as
// `<repository>:<tag>@sha256:…`. The tag must equal the tag of the image at
// the key path in the addon's values.yaml, without its digest, and the
// repository must be the same.
var guideImageRestatements = []struct {
	file, repository, addon string
	values                  []string
}{
	{"addons.md", "rancher/system-upgrade-controller", "system-upgrade-controller", []string{"systemUpgradeController", "image"}},
	{"addons.md", "rancher/kuberlr-kubectl", "system-upgrade-controller", []string{"kubectl", "image"}},
}

// guideNotPins are mentions of a pinned chart with a version that do not
// restate the pin. text is a part of the line that holds the mention. Each
// entry must still find its mention, so a stale entry fails.
var guideNotPins = []struct{ file, text, why string }{
	{"addons.md", "measured on cilium 1.19.2", "a measurement on an older release"},
	{"addons.md", "`envoy-gateway` v1.8.0 or later", "the first release with a feature"},
}

// chartPin is one ChartRenderer document of an addon: the chart (a name, or
// the path of a local chart), its version ("" for a local chart) and the
// file, relative to the addons tree.
type chartPin struct{ chart, version, file string }

// guideMention is a pin as the guide writes it: the file (under docs/guide),
// the line, the names it goes by (the chart, the addon) and the version as
// written.
type guideMention struct {
	file    string
	line    int
	names   []string
	version string
}

// readChartPins reads the ChartRenderer documents of each addon directory
// under addonsDir, keyed by the addon name. An addon without one has an
// empty entry. Only a YAML file that names the kind is parsed; such a file
// that does not parse fails the test.
func readChartPins(t *testing.T, addonsDir string) map[string][]chartPin {
	t.Helper()
	entries, err := os.ReadDir(addonsDir)
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
		err := filepath.WalkDir(filepath.Join(addonsDir, addon), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(raw, []byte("kind: ChartRenderer")) {
				return err
			}
			rel, _ := filepath.Rel(addonsDir, path)
			dec := yaml.NewDecoder(bytes.NewReader(raw))
			for {
				var doc struct {
					Kind    string `yaml:"kind"`
					Chart   string `yaml:"chart"`
					Version string `yaml:"version"`
				}
				err := dec.Decode(&doc)
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return fmt.Errorf("%s: %w", rel, err)
				}
				if doc.Kind == "ChartRenderer" {
					pins[addon] = append(pins[addon], chartPin{doc.Chart, doc.Version, filepath.ToSlash(rel)})
				}
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return pins
}

// readGuide reads a file under docs/guide.
func readGuide(t *testing.T, root, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, guideDir, file))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// checkGuideTable checks the rows of the "Framework addons" table. It
// returns the chart versions that it checked, the number of rows and a
// problem for each row that does not match its pins. A Chart cell is one of:
//
//   - "local chart": each ChartRenderer of the addon renders a chart from a
//     path of the addon ("./…"), with no version;
//   - "raw manifests": the addon has no ChartRenderer;
//   - a list of "`<repo>/<chart>` <version>", separated by ", ": each chart
//     has a ChartRenderer in the addon with that version, and each versioned
//     ChartRenderer of the addon is in the list. The table writes a v before
//     some versions that the chart pins without one, so a leading v is not
//     compared.
func checkGuideTable(t *testing.T, root string, pins map[string][]chartPin) (checked []guideMention, rows int, problems []string) {
	t.Helper()
	lines := strings.Split(readGuide(t, root, guideAddonsFile), "\n")
	start := slices.Index(lines, guideTableHead)
	if start < 0 {
		t.Fatalf("%s/%s: no line %q", guideDir, guideAddonsFile, guideTableHead)
	}
	inTable := false
	for i := start + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "#") {
			break
		}
		if !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			continue
		}
		inTable = true
		where := fmt.Sprintf("%s/%s:%d", guideDir, guideAddonsFile, i+1)
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
		rows++
		addonPins, ok := pins[addon]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: the addon %q is not in .lok8s/addons", where, addon))
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
			for _, item := range strings.Split(cell, ", ") {
				m := guideChartRefRE.FindStringSubmatch(item)
				if m == nil {
					problems = append(problems, fmt.Sprintf("%s: the chart %q is not \"`<repo>/<chart>` <version>\", \"local chart\" or \"raw manifests\"", where, item))
					continue
				}
				chart := m[1][strings.LastIndex(m[1], "/")+1:]
				named[chart] = true
				k := slices.IndexFunc(addonPins, func(p chartPin) bool { return p.chart == chart })
				if k < 0 {
					problems = append(problems, fmt.Sprintf("%s: %s has no ChartRenderer of the chart %q", where, addon, chart))
					continue
				}
				pin := addonPins[k]
				checked = append(checked, guideMention{guideAddonsFile, i + 1, []string{chart, addon}, m[2]})
				if strings.TrimPrefix(m[2], "v") != strings.TrimPrefix(pin.version, "v") {
					problems = append(problems, fmt.Sprintf("%s: %s %s, but .lok8s/addons/%s pins %s", where, chart, m[2], pin.file, pin.version))
				}
			}
			for _, p := range addonPins {
				if p.version != "" && !named[p.chart] {
					problems = append(problems, fmt.Sprintf("%s: the row of %s does not name the chart %s %s (.lok8s/addons/%s)", where, addon, p.chart, p.version, p.file))
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

// checkGuideRestatements checks guideRestatements and
// guideImageRestatements. It returns the versions that it checked and a
// problem for each entry that does not match.
func checkGuideRestatements(t *testing.T, root string, pins map[string][]chartPin) (checked []guideMention, problems []string) {
	t.Helper()
	for _, r := range guideRestatements {
		where := guideDir + "/" + r.file
		got, line, n := findOnce(regexp.MustCompile(r.re), readGuide(t, root, r.file))
		if n != 1 {
			problems = append(problems, fmt.Sprintf("%s: %d matches of %s, want 1: the restatement moved or was reworded, so update guideRestatements", where, n, r.re))
			continue
		}
		i := slices.IndexFunc(pins[r.addon], func(p chartPin) bool { return p.chart == r.chart })
		if i < 0 {
			problems = append(problems, fmt.Sprintf("%s:%d: the addon %s has no ChartRenderer of the chart %s", where, line, r.addon, r.chart))
			continue
		}
		pin := pins[r.addon][i]
		checked = append(checked, guideMention{r.file, line, []string{r.chart, r.addon}, got})
		if got != pin.version {
			problems = append(problems, fmt.Sprintf("%s:%d: %s %s, but .lok8s/addons/%s pins %s", where, line, r.addon, got, pin.file, pin.version))
		}
	}
	for _, r := range guideImageRestatements {
		where := guideDir + "/" + r.file
		re := regexp.MustCompile("`" + regexp.QuoteMeta(r.repository) + ":([^@`]+)@sha256:")
		got, line, n := findOnce(re, readGuide(t, root, r.file))
		if n != 1 {
			problems = append(problems, fmt.Sprintf("%s: %d matches of %s, want 1: the restatement moved or was reworded, so update guideImageRestatements", where, n, re))
			continue
		}
		repository, tag := valuesImage(t, filepath.Join(root, ".lok8s", "addons", r.addon, "values.yaml"), r.values)
		tag, _, _ = strings.Cut(tag, "@")
		name := r.repository[strings.LastIndex(r.repository, "/")+1:]
		checked = append(checked, guideMention{r.file, line, []string{name, r.addon}, got})
		if repository != r.repository || got != tag {
			problems = append(problems, fmt.Sprintf("%s:%d: %s:%s, but .lok8s/addons/%s/values.yaml %s pins %s:%s", where, line, r.repository, got, r.addon, strings.Join(r.values, "."), repository, tag))
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
	image, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("%s: %s is not a map", path, strings.Join(keys, "."))
	}
	repository, _ = image["repository"].(string)
	tag, _ = image["tag"].(string)
	if repository == "" || tag == "" {
		t.Fatalf("%s: %s has no repository or tag", path, strings.Join(keys, "."))
	}
	return repository, tag
}

// guideScanNames are the names that the scan looks for: each addon with a
// versioned ChartRenderer, and each chart that it pins.
func guideScanNames(pins map[string][]chartPin) []string {
	set := map[string]bool{}
	for addon, ps := range pins {
		for _, p := range ps {
			if p.version != "" {
				set[addon] = true
				set[p.chart] = true
			}
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	return names
}

// guideMentionRE finds a name followed by a version: the name, optional
// quotes or backticks, then a space, an @ or a colon, then the version. The
// name must not continue a longer name (cert-manager in
// cert-manager-webhook-hetzner).
func guideMentionRE(names []string) *regexp.Regexp {
	q := make([]string, 0, len(names))
	for _, n := range names {
		q = append(q, regexp.QuoteMeta(n))
	}
	// The longest name first, so that the alternation tries it first.
	sort.Slice(q, func(i, j int) bool { return len(q[i]) > len(q[j]) || (len(q[i]) == len(q[j]) && q[i] < q[j]) })
	return regexp.MustCompile("(?:^|[^A-Za-z0-9_-])(" + strings.Join(q, "|") + ")[`'\"]*(?:[ \\t]+|@|:[ \\t]*)(" + guideVersion + ")")
}

// guideHit is a mention that the scan found.
type guideHit struct {
	file          string
	line          int
	name, version string
	text          string // from the name to the end of the version
}

// scanGuide finds each mention of re in the Markdown files under dir.
func scanGuide(t *testing.T, dir string, re *regexp.Regexp) []guideHit {
	t.Helper()
	var hits []guideHit
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		for i, line := range strings.Split(string(raw), "\n") {
			for _, m := range re.FindAllStringSubmatchIndex(line, -1) {
				hits = append(hits, guideHit{filepath.ToSlash(rel), i + 1, line[m[2]:m[3]], line[m[4]:m[5]], line[m[2]:m[5]]})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

// uncheckedGuideHits returns each hit that no checked mention and no entry of
// notPins covers, and each entry of notPins that covers no hit. A checked
// mention covers a hit on its line with one of its names and its version. An
// entry of notPins covers a hit in its file whose text is a part of the
// entry's text, on a line that holds the entry's text.
func uncheckedGuideHits(hits []guideHit, checked []guideMention, notPins []struct{ file, text, why string }, lines func(file string, line int) string) (unchecked []guideHit, stale []string) {
	used := make([]bool, len(notPins))
	for _, h := range hits {
		if slices.ContainsFunc(checked, func(c guideMention) bool {
			return c.file == h.file && c.line == h.line && c.version == h.version && slices.Contains(c.names, h.name)
		}) {
			continue
		}
		covered := false
		for i, np := range notPins {
			if np.file == h.file && strings.Contains(np.text, h.text) && strings.Contains(lines(h.file, h.line), np.text) {
				used[i] = true
				covered = true
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

func TestGuideAddonTableMatchesThePins(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	pins := readChartPins(t, filepath.Join(root, ".lok8s", "addons"))
	checked, rows, problems := checkGuideTable(t, root, pins)
	for _, p := range problems {
		t.Error(p)
	}
	if rows < guideTableRows {
		t.Errorf("%s/%s: %d rows in the %q table, want at least %d. A row was removed, or the parser does not see it. "+
			"If a row was removed on purpose, lower guideTableRows to %d", guideDir, guideAddonsFile, rows, guideTableHead, guideTableRows, rows)
	}
	if len(checked) == 0 {
		t.Error("the table check compared no chart version")
	}
}

func TestGuideRestatementsMatchThePins(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	pins := readChartPins(t, filepath.Join(root, ".lok8s", "addons"))
	_, problems := checkGuideRestatements(t, root, pins)
	for _, p := range problems {
		t.Error(p)
	}
}

func TestGuideNamesNoUncheckedPin(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	pins := readChartPins(t, filepath.Join(root, ".lok8s", "addons"))
	table, _, _ := checkGuideTable(t, root, pins)
	restated, _ := checkGuideRestatements(t, root, pins)
	hits := scanGuide(t, filepath.Join(root, guideDir), guideMentionRE(guideScanNames(pins)))

	// The table is in the form that the scan finds: a table entry that the
	// scan misses means that the scan pattern is broken.
	for _, c := range table {
		if !slices.ContainsFunc(hits, func(h guideHit) bool {
			return h.file == c.file && h.line == c.line && h.version == c.version && slices.Contains(c.names, h.name)
		}) {
			t.Errorf("%s/%s:%d: the scan does not find the table entry %s %s, so guideMentionRE is broken", guideDir, c.file, c.line, c.names[0], c.version)
		}
	}

	files := map[string][]string{}
	lines := func(file string, line int) string {
		if _, ok := files[file]; !ok {
			files[file] = strings.Split(readGuide(t, root, file), "\n")
		}
		return files[file][line-1]
	}
	unchecked, stale := uncheckedGuideHits(hits, append(table, restated...), guideNotPins, lines)
	for _, h := range unchecked {
		t.Errorf("%s/%s:%d: %q names a pinned chart with a version, and no test checks it. "+
			"If it restates the pin, add it to guideRestatements (or guideImageRestatements). If not, add it to guideNotPins with the reason",
			guideDir, h.file, h.line, h.text)
	}
	for _, s := range stale {
		t.Errorf("guideNotPins: %s is not in the guide any more: remove the entry", s)
	}
}

// The scan logic on a fixture: a new mention of a pinned chart is unchecked,
// a checked mention and a listed one are not, and a listed mention that is
// gone is stale.
func TestGuideScanFindsAnUncheckedMention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.WriteFile(t, filepath.Join(dir, "a.md"), "| `cilium` | CNI | `cilium/cilium` v1.20.4 |\n"+
		"Upgrade to cilium 1.20.9 first.\n"+
		"It was measured on cilium 1.19.2.\n"+
		"`cert-manager-webhook-hetzner` 0.7.0 needs `cert-manager`.\n")
	hits := scanGuide(t, dir, guideMentionRE([]string{"cilium", "cert-manager", "cert-manager-webhook-hetzner"}))
	got := make([]string, 0, len(hits))
	for _, h := range hits {
		got = append(got, fmt.Sprintf("%s:%d %s %s", h.file, h.line, h.name, h.version))
	}
	want := []string{"a.md:1 cilium v1.20.4", "a.md:2 cilium 1.20.9", "a.md:3 cilium 1.19.2", "a.md:4 cert-manager-webhook-hetzner 0.7.0"}
	if !slices.Equal(got, want) {
		t.Fatalf("hits: got %q, want %q", got, want)
	}

	raw := strings.Split("| `cilium` | CNI | `cilium/cilium` v1.20.4 |\nUpgrade to cilium 1.20.9 first.\nIt was measured on cilium 1.19.2.\n", "\n")
	lines := func(_ string, line int) string { return raw[line-1] }
	checked := []guideMention{{"a.md", 1, []string{"cilium"}, "v1.20.4"}}
	notPins := []struct{ file, text, why string }{
		{"a.md", "measured on cilium 1.19.2", "a measurement"},
		{"a.md", "metallb 0.1.0", "gone"},
	}
	unchecked, stale := uncheckedGuideHits(hits[:3], checked, notPins, lines)
	if len(unchecked) != 1 || unchecked[0].text != "cilium 1.20.9" {
		t.Errorf("unchecked: got %+v, want only cilium 1.20.9", unchecked)
	}
	if !slices.Equal(stale, []string{"a.md: metallb 0.1.0"}) {
		t.Errorf("stale: got %q, want the metallb entry", stale)
	}
}
