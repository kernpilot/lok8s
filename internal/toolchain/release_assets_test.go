package toolchain

// release_assets_test.go: the asset-name contract of a lok8s release.
// Three consumers address the assets goreleaser publishes by name, and
// none of them sees the others: install/lo-install.sh composes
// `<member>-<os>-<arch>.tar.gz`, the contributor .bin/b.yaml selects the
// core archive with a glob, and the consumer template (template.go)
// selects the Secret plugin with another. This test renders the
// .goreleaser.yaml name templates and runs both globs through
// filepath.Match, the matcher b uses, so a rename on any side fails here.
//
// The core glob must NOT match the lo-full archives: b scores
// `lo-linux-amd64.tar.gz` and `lo-full-linux-amd64.tar.gz` the same, and
// the release lists lo-full first, so a glob that admits both installs
// lo-full under the `lo` alias.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/testutil"
)

type goreleaserConfig struct {
	Archives []struct {
		ID           string   `yaml:"id"`
		Formats      []string `yaml:"formats"`
		NameTemplate string   `yaml:"name_template"`
	} `yaml:"archives"`
	Checksum struct {
		NameTemplate string `yaml:"name_template"`
	} `yaml:"checksum"`
	Release struct {
		ExtraFiles []struct {
			Glob string `yaml:"glob"`
		} `yaml:"extra_files"`
	} `yaml:"release"`
}

// releaseAssetNames renders every archive name for the linux/darwin x
// amd64/arm64 matrix (tar.gz archives get the suffix, raw binaries none).
func releaseAssetNames(t *testing.T, cfg goreleaserConfig) []string {
	t.Helper()
	var names []string
	for _, a := range cfg.Archives {
		if len(a.Formats) != 1 {
			t.Fatalf("archive %q: want exactly one format, got %v", a.ID, a.Formats)
		}
		suffix := ""
		if a.Formats[0] == "tar.gz" {
			suffix = ".tar.gz"
		}
		for _, goos := range []string{"linux", "darwin"} {
			for _, arch := range []string{"amd64", "arm64"} {
				n := strings.NewReplacer("{{ .Os }}", goos, "{{ .Arch }}", arch).Replace(a.NameTemplate)
				if strings.Contains(n, "{{") {
					t.Fatalf("archive %q: unrendered template in %q", a.ID, n)
				}
				names = append(names, n+suffix)
			}
		}
	}
	return names
}

// matching returns the names a glob selects, through the matcher b uses.
func matching(t *testing.T, glob string, names []string) []string {
	t.Helper()
	var out []string
	for _, n := range names {
		ok, err := filepath.Match(glob, n)
		if err != nil {
			t.Fatalf("glob %q: %v", glob, err)
		}
		if ok {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

func matrix(prefix, suffix string) []string {
	return []string{
		prefix + "darwin-amd64" + suffix,
		prefix + "darwin-arm64" + suffix,
		prefix + "linux-amd64" + suffix,
		prefix + "linux-arm64" + suffix,
	}
}

// contributorAssetGlob reads the `asset:` of the github.com/kernpilot/lok8s
// entry in the contributor .bin/b.yaml (the one with `alias: lo`).
func contributorAssetGlob(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, ".bin", "b.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Binaries map[string]struct {
			Alias string `yaml:"alias"`
			Asset string `yaml:"asset"`
		} `yaml:"binaries"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	e, ok := doc.Binaries["github.com/kernpilot/lok8s"]
	if !ok || e.Alias != "lo" || e.Asset == "" {
		t.Fatalf(".bin/b.yaml: want a github.com/kernpilot/lok8s entry with alias lo and an asset glob, got %+v", e)
	}
	return e.Asset
}

// templateAssetGlob reads the `asset:` the consumer template pins for the
// Secret plugin.
func templateAssetGlob(t *testing.T) string {
	t.Helper()
	for _, e := range entries("0.3.0", "") {
		if e.key != "github.com/kernpilot/lok8s" {
			continue
		}
		for _, f := range e.fields {
			if v, ok := strings.CutPrefix(f, "asset: "); ok {
				return v
			}
		}
	}
	t.Fatal("template.go: no asset field on the github.com/kernpilot/lok8s entry")
	return ""
}

func TestReleaseAssetNamesAreTheContract(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg goreleaserConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	names := releaseAssetNames(t, cfg)

	// The whole matrix, as the installer, b and the docs name it.
	want := slices.Concat(
		matrix("lo-", ".tar.gz"),
		matrix("lo-full-", ".tar.gz"),
		matrix("kustomize-secret-", ""),
		matrix("lochat-", ""),
	)
	slices.Sort(want)
	got := slices.Clone(names)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf(".goreleaser.yaml archive names = %v, want %v", got, want)
	}

	// The installer composes `<member>-<os>-<arch>.tar.gz` and verifies it
	// against checksums.txt; goreleaser must publish both under these names.
	installer, err := os.ReadFile(filepath.Join(root, "install", "lo-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		`local asset="${member}-${os}-${arch}.tar.gz"`,
		`local member="lo" flavor="core"`,
		`member="lo-full"; flavor="full"`,
		`/checksums.txt`,
	} {
		if !strings.Contains(string(installer), needle) {
			t.Errorf("install/lo-install.sh no longer contains %q: the asset names it composes drifted from .goreleaser.yaml", needle)
		}
	}
	if cfg.Checksum.NameTemplate != "checksums.txt" {
		t.Errorf("checksum.name_template = %q, want checksums.txt (the name the installer and the README fetch)", cfg.Checksum.NameTemplate)
	}
	var extra []string
	for _, f := range cfg.Release.ExtraFiles {
		extra = append(extra, f.Glob)
	}
	if !slices.Contains(extra, "install/lo-install.sh") {
		t.Errorf("release.extra_files = %v: install/lo-install.sh is not published, so releases/latest/download/lo-install.sh is a 404", extra)
	}

	// The contributor b.yaml selects the core archive, never lo-full.
	coreGlob := contributorAssetGlob(t, root)
	if got, want := matching(t, coreGlob, names), matrix("lo-", ".tar.gz"); !slices.Equal(got, want) {
		t.Errorf(".bin/b.yaml asset %q selects %v, want exactly %v (a glob that admits lo-full-* installs lo-full under the lo alias)", coreGlob, got, want)
	}

	// The consumer template selects the Secret plugin binaries.
	secretGlob := templateAssetGlob(t)
	if got, want := matching(t, secretGlob, names), matrix("kustomize-secret-", ""); !slices.Equal(got, want) {
		t.Errorf("template.go asset %q selects %v, want exactly %v", secretGlob, got, want)
	}
}
