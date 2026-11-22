package toolchain

// shellcheck_test.go: the shellcheck entry of the contributor .bin/b.yaml.
// It is the local lint of one shell file before CI (TESTING.md, "Shell
// lint"). So it must be the release that the CI lint container runs, and no
// env-sync profile may give it to a consumer.

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// shellcheckEntry is the key of the shellcheck entry in .bin/b.yaml.
const shellcheckEntry = "github.com/koalaman/shellcheck"

// The patterns of the CI sources: group 1 is the release. CI pins the argsh
// container by its digest (hack/lint-shell.sh). These two comments name the
// shellcheck that the image bundles, and a digest bump updates them.
const (
	ciShellcheckPattern   = `\(shellcheck ([0-9][^ )]*) inside\)`
	lintShellcheckPattern = `the shellcheck v([^ ]+) \+ argsh-lint`
)

// The sources of the shellcheck release.
var (
	byamlShellcheck = pinSource{file: ".bin/b.yaml", prefix: "v", read: byamlVersion(shellcheckEntry)}
	ciShellcheck    = pinSource{file: ".github/workflows/ci.yml", read: matchPin(ciShellcheckPattern)}
	lintShellcheck  = pinSource{file: "hack/lint-shell.sh", read: matchPin(lintShellcheckPattern)}
)

// byamlVersion returns a reader of a .bin/b.yaml: the version of its
// binaries entry key.
func byamlVersion(key string) func(raw []byte) (string, error) {
	return func(raw []byte) (string, error) {
		var doc struct {
			Binaries map[string]*struct {
				Version string `yaml:"version"`
			} `yaml:"binaries"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return "", err
		}
		e, ok := doc.Binaries[key]
		if !ok {
			return "", fmt.Errorf("no binaries entry %s", key)
		}
		if e == nil {
			return "", fmt.Errorf("the binaries entry %s has no version", key)
		}
		return e.Version, nil
	}
}

// TestShellcheckPinIsTheCIRelease: .bin/b.yaml pins the shellcheck that
// the CI lint container runs, so a local lint gives the findings of CI.
func TestShellcheckPinIsTheCIRelease(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	ci := releaseOf(t, root, ciShellcheck)
	testutil.Drift{
		Want: testutil.Tree{Name: "the shellcheck that " + ciShellcheck.file + " names", Files: map[string]string{
			byamlShellcheck.file: ci,
			lintShellcheck.file:  ci,
		}},
		Got: testutil.Tree{Name: "the other shellcheck sources", Files: map[string]string{
			byamlShellcheck.file: releaseOf(t, root, byamlShellcheck),
			lintShellcheck.file:  releaseOf(t, root, lintShellcheck),
		}},
		Sync: "bump the shellcheck of .bin/b.yaml and the comments of hack/lint-shell.sh and ci.yml with the argsh image digest, then run ./.bin/b install " + shellcheckEntry + " to refresh .bin/b.lock",
	}.Check(t)
}

// TestShellcheckSourcesFailWithoutThePin is the control of the pin test.
// Each source gives a release from the real file. On the same file
// without the pin, its reader fails with the message that the pin test
// prints. Thus a pin test that passes has read each pin.
func TestShellcheckSourcesFailWithoutThePin(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	for _, tc := range []struct {
		src  pinSource
		drop func(t *testing.T, raw []byte) ([]byte, int)
		want string
	}{
		{byamlShellcheck, dropBYamlEntry(shellcheckEntry),
			".bin/b.yaml: no binaries entry " + shellcheckEntry},
		{ciShellcheck, dropLines("inside), via the"),
			".github/workflows/ci.yml: 0 matches of " + ciShellcheckPattern + ", want one"},
		{lintShellcheck, dropLines("+ argsh-lint"),
			"hack/lint-shell.sh: 0 matches of " + lintShellcheckPattern + ", want one"},
	} {
		t.Run(tc.src.file, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(tc.src.file)))
			if err != nil {
				t.Fatal(err)
			}
			if got, err := tc.src.parse(raw); err != nil || got == "" {
				t.Fatalf("the real %s gives %q (%v), want a release", tc.src.file, got, err)
			}
			cut, n := tc.drop(t, raw)
			if n == 0 {
				t.Fatalf("the control removed nothing from %s", tc.src.file)
			}
			if _, err := tc.src.parse(cut); err == nil || err.Error() != tc.want {
				t.Errorf("%s without the pin: error %v, want %q", tc.src.file, err, tc.want)
			}
		})
	}
}

// dropBYamlEntry returns a drop of the binaries entry key of a .bin/b.yaml,
// and the number of entries it removed.
func dropBYamlEntry(key string) func(*testing.T, []byte) ([]byte, int) {
	return func(t *testing.T, raw []byte) ([]byte, int) {
		t.Helper()
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		bins, ok := doc["binaries"].(map[string]any)
		if !ok {
			t.Fatal(".bin/b.yaml has no binaries map")
		}
		n := 0
		if _, ok := bins[key]; ok {
			delete(bins, key)
			n = 1
		}
		out, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return out, n
	}
}

// TestShellcheckIsInNoProfile: no env-sync profile of .bin/b.yaml gives the
// shellcheck entry to a consumer.
func TestShellcheckIsInNoProfile(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), ".bin", "b.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profileProblems(raw, shellcheckEntry) {
		t.Error(p)
	}
}

// profileSelectRE is the one form of a profile's .bin/b.yaml select that
// profileProblems can read: the entries whose groups contain one group.
var profileSelectRE = regexp.MustCompile(`^\{binaries: from_items\(items\(binaries\)\[\?\[1\]\.groups && contains\(\[1\]\.groups, '([^']+)'\)\]\)\}$`)

// profileProblems returns why a profile of the b.yaml raw can give the
// binaries entry key to a consumer, or nil. A profile gives it when its
// select picks a group of the entry, or when it syncs .bin/b.yaml without
// a select. A select of another form is a problem too: this check cannot
// read it.
func profileProblems(raw []byte, key string) []string {
	var doc struct {
		Binaries map[string]*struct {
			Groups []string `yaml:"groups"`
		} `yaml:"binaries"`
		Profiles map[string]struct {
			Files map[string]*struct {
				Select []string `yaml:"select"`
			} `yaml:"files"`
		} `yaml:"profiles"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return []string{"b.yaml does not parse: " + err.Error()}
	}
	entry, ok := doc.Binaries[key]
	if !ok {
		return []string{"b.yaml has no binaries entry " + key}
	}
	var groups []string
	if entry != nil {
		groups = entry.Groups
	}
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(doc.Profiles)) {
		files := doc.Profiles[name].Files
		for _, file := range slices.Sorted(maps.Keys(files)) {
			spec := files[file]
			if file != ".bin/b.yaml" {
				if strings.HasPrefix(file, ".bin/") || strings.HasPrefix(file, "*") {
					problems = append(problems, fmt.Sprintf("profile %s: the file pattern %s can sync .bin/b.yaml without a select", name, file))
				}
				continue
			}
			if spec == nil || len(spec.Select) == 0 {
				problems = append(problems, fmt.Sprintf("profile %s syncs all of .bin/b.yaml", name))
				continue
			}
			for _, sel := range spec.Select {
				m := profileSelectRE.FindStringSubmatch(sel)
				if m == nil {
					problems = append(problems, fmt.Sprintf("profile %s: a select of another form: %s", name, sel))
					continue
				}
				if slices.Contains(groups, m[1]) {
					problems = append(problems, fmt.Sprintf("profile %s selects %s through the group %s", name, key, m[1]))
				}
			}
		}
	}
	return problems
}

// TestProfileProblemsFindEachLeak: profileProblems reports each way that a
// profile can give an entry to a consumer, and nothing for a clean file.
func TestProfileProblemsFindEachLeak(t *testing.T) {
	t.Parallel()
	const sel = `"{binaries: from_items(items(binaries)[?[1].groups && contains([1].groups, 'core')])}"`
	profile := func(files string) string {
		return "profiles:\n  core:\n    files:\n" + files
	}
	selected := "      .bin/b.yaml:\n        select:\n          - " + sel + "\n"
	for _, tc := range []struct {
		name, byaml string
		want        []string
	}{
		{"clean", "binaries:\n  x/y:\n    groups: [lint]\n" + profile(selected), nil},
		{"no groups", "binaries:\n  x/y: {}\n" + profile(selected), nil},
		{"in the core group", "binaries:\n  x/y:\n    groups: [lint, core]\n" + profile(selected),
			[]string{"profile core selects x/y through the group core"}},
		{"the whole file", "binaries:\n  x/y:\n    groups: [lint]\n" + profile("      .bin/b.yaml:\n"),
			[]string{"profile core syncs all of .bin/b.yaml"}},
		{"a glob", "binaries:\n  x/y:\n    groups: [lint]\n" + profile(selected+"      .bin/**:\n"),
			[]string{"profile core: the file pattern .bin/** can sync .bin/b.yaml without a select"}},
		{"another select form", "binaries:\n  x/y:\n    groups: [lint]\n" + profile("      .bin/b.yaml:\n        select:\n          - \"{binaries: binaries}\"\n"),
			[]string{"profile core: a select of another form: {binaries: binaries}"}},
		{"no entry", "binaries:\n  a/b:\n    groups: [lint]\n" + profile(selected),
			[]string{"b.yaml has no binaries entry x/y"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := profileProblems([]byte(tc.byaml), "x/y"); !slices.Equal(got, tc.want) {
				t.Errorf("profileProblems = %q, want %q", got, tc.want)
			}
		})
	}
}
