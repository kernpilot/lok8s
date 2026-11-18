package toolchain

// bpin_test.go: the CI workflows (.github/workflows) and the contributor
// lock (.bin/b.lock, with the committed .bin/b) pin the b release that
// BRelease names. The workflows download the b tarball with curl and check
// it against a sha256 written in the step; the lock records the b that
// `b install` keeps, and .bin/b is that binary. A b bump changes BRelease,
// these pins, the lock and .bin/b in one commit (and the generated
// docs/reference/cli-commands.md, which TestCommittedReferenceIsCurrent
// checks). These tests fail on a workflow or the lock left behind. They do
// not read other files that name a b version, such as release notes.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

var (
	// bVersionRE is an assignment of the b version: B_VERSION=4.18.8 in a
	// run script, or B_VERSION: "4.18.8" in an env block.
	bVersionRE = regexp.MustCompile(`^\s*B_VERSION\s*[=:]\s*"?([^"\s]*)"?\s*$`)
	// bURLRE is a b release asset URL: the version from B_VERSION or
	// written in the URL.
	bURLRE = regexp.MustCompile(`https://github\.com/fentas/b/releases/download/(v\$\{B_VERSION\}|v\$B_VERSION|v[^/"\s]+)/(b-[a-z0-9]+-[a-z0-9]+\.tar\.gz)`)
	// bOutRE is the file curl writes the asset to.
	bOutRE = regexp.MustCompile(`\s-o\s+"?([^"\s]+)"?`)
	// sumCheckRE is a sha256 check of one file, a command at the start of
	// its line: echo "<sum>  <file>" | sha256sum -c -. Only words can
	// follow sha256sum, so a shell list (|| true, || :, && …) after it
	// does not match. sumCheck requires the check flag in those words.
	sumCheckRE = regexp.MustCompile(`^\s*echo\s+"([0-9a-f]{64})\s+([^"\s]+)"\s*\|\s*sha256sum((?:\s+[^\s|&;]+)*)\s*$`)
)

// sumCheck returns the sum and the file of a sha256 check on line, and ok
// false when line is not a check that can fail the step: a comment, a
// sha256sum with no -c (or --check) flag, which prints the sum of its
// input and passes, or a check whose failure a shell list after it
// catches.
func sumCheck(line string) (sum, file string, ok bool) {
	m := sumCheckRE.FindStringSubmatch(line)
	if m == nil || !slices.ContainsFunc(strings.Fields(m[3]), func(a string) bool { return a == "-c" || a == "--check" }) {
		return "", "", false
	}
	return m[1], m[2], true
}

// stepWindow is the number of lines before a download in which its
// B_VERSION must stand, and after it in which its sha256 check must stand.
// A step that took B_VERSION from an earlier step would pass unread.
const stepWindow = 3

// bPinProblems returns how one workflow file breaks the rule against rel,
// and the number of b downloads it found. Each B_VERSION assignment is
// rel.Version. Each line that names a b release URL is a download whose
// version is rel.Version (from a B_VERSION in the stepWindow lines before
// it, or written in the URL), whose asset rel pins, which writes to a file
// (-o), and whose next stepWindow lines check that file against the sum
// rel pins for the asset. A line that names the b release path in
// another form is a problem too, so a change of the step's form fails
// here instead of passing unread.
func bPinProblems(name, text string, rel Release) (downloads int, problems []string) {
	lines := strings.Split(text, "\n")
	version, versionAt := "", 0
	for i, line := range lines {
		at := fmt.Sprintf("%s:%d", name, i+1)
		if m := bVersionRE.FindStringSubmatch(line); m != nil {
			version, versionAt = m[1], i+1
			if m[1] != rel.Version {
				problems = append(problems, fmt.Sprintf("%s sets B_VERSION=%s, want %s (internal/toolchain BRelease)", at, m[1], rel.Version))
			}
			continue
		}
		if !strings.Contains(line, "fentas/b/releases") {
			continue
		}
		m := bURLRE.FindStringSubmatch(line)
		if m == nil {
			problems = append(problems, fmt.Sprintf("%s names a b release in a form this test does not read: write the step as the others are", at))
			continue
		}
		downloads++
		got := strings.TrimPrefix(m[1], "v")
		if strings.Contains(m[1], "B_VERSION") {
			if versionAt == 0 || i+1-versionAt > stepWindow {
				problems = append(problems, fmt.Sprintf("%s downloads b v${B_VERSION} with no B_VERSION in the %d lines before it", at, stepWindow))
				continue
			}
			got = version
		}
		if got != rel.Version {
			problems = append(problems, fmt.Sprintf("%s downloads b %s, want %s (internal/toolchain BRelease)", at, got, rel.Version))
		}
		want, pinned := rel.Assets[m[2]]
		if !pinned {
			problems = append(problems, fmt.Sprintf("%s downloads %s, which BRelease does not pin", at, m[2]))
			continue
		}
		out := bOutRE.FindStringSubmatch(line)
		if out == nil {
			problems = append(problems, fmt.Sprintf("%s downloads %s with no -o file, so no sha256 check names it", at, m[2]))
			continue
		}
		checked := false
		for _, next := range lines[i+1 : min(i+1+stepWindow, len(lines))] {
			sum, file, ok := sumCheck(next)
			if !ok || file != out[1] {
				continue
			}
			checked = true
			if sum != want {
				problems = append(problems, fmt.Sprintf("%s checks %s against sha256 %s, want %s (internal/toolchain BRelease, %s)", at, out[1], sum, want, m[2]))
			}
		}
		if !checked {
			problems = append(problems, fmt.Sprintf("%s downloads %s to %s with no sha256 check of that file in the next %d lines: "+
				`write the line echo "<sum>  %s" | sha256sum -c - with nothing after it`, at, m[2], out[1], stepWindow, out[1]))
		}
	}
	return downloads, problems
}

// bPinFloors is the number of b downloads each workflow holds today. It is
// a floor, so a new download needs no change here. A file with fewer fails:
// a step was removed or reworded, or the walk skips the file. Without the
// floor a step written in another form would not be checked at all.
var bPinFloors = map[string]int{"ci.yml": 4, "e2e.yml": 1, "provision.yml": 1, "spinup.yml": 1}

// bPinFloorProblem returns how n, the b downloads found in the workflow
// name, falls below the floor of that workflow, or "" when it does not.
func bPinFloorProblem(name string, n int) string {
	if n >= bPinFloors[name] {
		return ""
	}
	return fmt.Sprintf("%s: %d b downloads, want at least %d. A step was removed, or reworded so that this test does not find it, "+
		"or the walk skips the file. If a step was removed on purpose, lower the floor of %s in bPinFloors to %d",
		name, n, bPinFloors[name], name, n)
}

// TestWorkflowsInstallThePinnedB: every workflow that downloads b
// downloads BRelease and checks its sum.
func TestWorkflowsInstallThePinnedB(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(testutil.RepoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, e := range entries {
		if e.IsDir() || (filepath.Ext(e.Name()) != ".yml" && filepath.Ext(e.Name()) != ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		n, problems := bPinProblems(e.Name(), string(raw), BRelease)
		found[e.Name()] = n
		for _, p := range problems {
			t.Error(p)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(bPinFloors)) {
		if p := bPinFloorProblem(name, found[name]); p != "" {
			t.Error(p)
		}
	}
}

// ciPinProblems is what TestWorkflowsInstallThePinnedB finds in text as
// ci.yml: the problems of bPinProblems, and the floor of ci.yml.
func ciPinProblems(text string) []string {
	n, problems := bPinProblems("ci.yml", text, BRelease)
	if p := bPinFloorProblem("ci.yml", n); p != "" {
		problems = append(problems, p)
	}
	return problems
}

// Each rule of bPinProblems and the floor fail on an edit of a copy of the
// real ci.yml. One more download step passes: the floor is a minimum, not
// an exact count.
func TestBPinCheckFindsEachHole(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	ci := string(raw)
	if problems := ciPinProblems(ci); len(problems) > 0 {
		t.Fatalf("the real ci.yml: problems %q", problems)
	}
	sum := BRelease.Assets["b-linux-amd64.tar.gz"]
	setVersion := "B_VERSION=" + BRelease.Version + "\n"
	url := `"https://github.com/fentas/b/releases/download/v${B_VERSION}/b-linux-amd64.tar.gz" -o /tmp/b.tar.gz`
	check := `echo "` + sum + `  /tmp/b.tar.gz" | sha256sum -c -`
	// step is the first download step: its B_VERSION line, the download
	// and the sum check.
	lines := strings.Split(ci, "\n")
	at := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, url) })
	if at < 1 || at+1 >= len(lines) {
		t.Fatalf("ci.yml has no %q: change this test with the workflow", url)
	}
	step := strings.Join(lines[at-1:at+2], "\n")
	for _, s := range []string{setVersion, check} {
		if !strings.Contains(step+"\n", s) {
			t.Fatalf("the first b download step of ci.yml has no %q: change this test with the workflow:\n%s", s, step)
		}
	}
	other := strings.Repeat("0", 64)
	cases := []struct {
		// want is a part of the problem the edit must cause; "" is an edit
		// that must pass.
		name, text, want string
	}{
		{"an old B_VERSION", strings.Replace(ci, setVersion, "B_VERSION=4.0.0\n", 1), "sets B_VERSION=4.0.0"},
		{"an old version written in the URL",
			strings.Replace(ci, url, strings.Replace(url, "v${B_VERSION}", "v4.0.0", 1), 1), "downloads b 4.0.0"},
		{"another sum", strings.Replace(ci, check, strings.Replace(check, sum, other, 1), 1), "against sha256 " + other},
		{"no sum check", strings.Replace(ci, check, "true", 1), "with no sha256 check"},
		{"a sum check of another file",
			strings.Replace(ci, check, strings.Replace(check, "/tmp/b.tar.gz", "/tmp/other.tar.gz", 1), 1), "with no sha256 check"},
		{"a sum check in a comment", strings.Replace(ci, check, "# "+check, 1), "with no sha256 check"},
		{"a sum check whose failure || true catches", strings.Replace(ci, check, check+" || true", 1), "with no sha256 check"},
		{"a sum check whose failure || : catches", strings.Replace(ci, check, check+" || :", 1), "with no sha256 check"},
		{"a sum check whose failure && catches", strings.Replace(ci, check, check+" && echo ok", 1), "with no sha256 check"},
		{"a sha256sum with no check flag",
			strings.Replace(ci, check, strings.TrimSuffix(check, " -c -"), 1), "with no sha256 check"},
		{"an asset that BRelease does not pin",
			strings.Replace(ci, url, strings.Replace(url, "b-linux-amd64", "b-darwin-amd64", 1), 1), "which BRelease does not pin"},
		{"a download with no -o file",
			strings.Replace(ci, url, strings.Replace(url, " -o /tmp/b.tar.gz", "", 1), 1), "with no -o file"},
		{"a URL with no B_VERSION before it",
			strings.ReplaceAll(ci, setVersion, "\n"), "with no B_VERSION in the"},
		{"a URL that takes B_VERSION from an earlier step",
			strings.Replace(ci, setVersion, setVersion+"\n\n\n", 1), "with no B_VERSION in the"},
		{"a b release in another form",
			strings.Replace(ci, url, `"https://github.com/fentas/b/releases/latest/download/b-linux-amd64.tar.gz" -o /tmp/b.tar.gz`, 1),
			"in a form this test does not read"},
		// The real ci.yml can hold more steps than the floor, so this text
		// holds one step fewer than the floor, whatever ci.yml holds.
		{"one download step fewer than the floor",
			strings.Repeat(step+"\n", bPinFloors["ci.yml"]-1), "want at least"},
		{"one download step more, checked with --check",
			strings.Replace(ci, step, step+"\n"+strings.Replace(step, "sha256sum -c -", "sha256sum --check -", 1), 1), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if c.text == ci {
				t.Fatal("the edit changed nothing: change this test with the workflow")
			}
			problems := ciPinProblems(c.text)
			if c.want == "" {
				if len(problems) > 0 {
					t.Errorf("the check fails a valid edit: %q", problems)
				}
				return
			}
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, c.want) }) {
				t.Errorf("the check lets this hole pass, want a problem with %q; it found %q", c.want, problems)
			}
		})
	}
}

// bLockProblems returns how the contributor lock and the committed .bin/b
// break the rule: the lock's b entry is rel.Version, and the sha256 of the
// committed binary is the one the lock records.
func bLockProblems(lock []byte, binary []byte, rel Release) []string {
	var doc struct {
		Binaries []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			SHA256  string `json:"sha256"`
		} `json:"binaries"`
	}
	if err := json.Unmarshal(lock, &doc); err != nil {
		return []string{".bin/b.lock is not JSON: " + err.Error()}
	}
	var problems []string
	n := 0
	for _, b := range doc.Binaries {
		if b.Name != "b" {
			continue
		}
		n++
		if b.Version != "v"+rel.Version {
			problems = append(problems, fmt.Sprintf(".bin/b.lock records b %s, want v%s (internal/toolchain BRelease): run ./.bin/b update b", b.Version, rel.Version))
		}
		sum := sha256.Sum256(binary)
		if got := hex.EncodeToString(sum[:]); got != b.SHA256 {
			problems = append(problems, fmt.Sprintf(".bin/b has sha256 %s, but .bin/b.lock records %s for b %s: commit the binary that ./.bin/b update b installs", got, b.SHA256, b.Version))
		}
	}
	if n != 1 {
		problems = append(problems, fmt.Sprintf(".bin/b.lock has %d entries named b, want one", n))
	}
	return problems
}

// TestContributorLockPinsTheB: the lock records BRelease, and .bin/b is
// the binary it records.
func TestContributorLockPinsTheB(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	lock, err := os.ReadFile(filepath.Join(root, ".bin", "b.lock"))
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(filepath.Join(root, ".bin", "b"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range bLockProblems(lock, binary, BRelease) {
		t.Error(p)
	}
	// Each rule fails on an edit of a copy.
	for _, c := range []struct {
		name         string
		lock, binary []byte
		want         string
	}{
		{"an old lock entry", []byte(strings.Replace(string(lock), `"version": "v`+BRelease.Version+`"`, `"version": "v4.0.0"`, 1)), binary, "records b v4.0.0"},
		{"another binary", lock, append(slices.Clone(binary), 0), "but .bin/b.lock records"},
		{"no b entry", []byte(strings.Replace(string(lock), `"name": "b"`, `"name": "not-b"`, 1)), binary, "0 entries named b"},
	} {
		if string(c.lock) == string(lock) && string(c.binary) == string(binary) {
			t.Fatalf("%s: the edit changed nothing: change this test with the lock", c.name)
		}
		problems := bLockProblems(c.lock, c.binary, BRelease)
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, c.want) }) {
			t.Errorf("%s: the check lets this hole pass, want a problem with %q; it found %q", c.name, c.want, problems)
		}
	}
}
