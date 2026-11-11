package assets

// version_test.go covers the version fallback to the embedded VERSION.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// The operator image builds lo from this tree with GOTOOLCHAIN=local, so its
// golang tag must equal the root go.mod `go` line. An older tag cannot build
// the module. A newer one tests another Go than CI and the release, which
// read go.mod. Dependabot does not follow this tag.
func TestOperatorImageGoMatchesGoMod(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	mod := regexp.MustCompile(`(?m)^go (\S+)$`).FindStringSubmatch(read("go.mod"))
	img := regexp.MustCompile(`(?m)^FROM golang:([0-9][^-@ ]*)`).FindAllStringSubmatch(read("operator/Dockerfile"), -1)
	if mod == nil {
		t.Fatal("go.mod has no `go` line")
	}
	if len(img) == 0 {
		t.Fatal("operator/Dockerfile has no `FROM golang:<version>` line")
	}
	for _, m := range img {
		if m[1] != mod[1] {
			t.Errorf("operator/Dockerfile uses golang:%s, go.mod says go %s", m[1], mod[1])
		}
	}
}

func TestVersionFallsBackToEmbedded(t *testing.T) {
	prev := BuildVersion
	t.Cleanup(func() { BuildVersion = prev })
	BuildVersion = ""
	emb, _ := readEmbedded("VERSION")
	if Version() != strings.TrimSpace(string(emb)) {
		t.Errorf("Version() = %q, embedded VERSION = %q", Version(), emb)
	}
	BuildVersion = "9.9.9"
	if Version() != "9.9.9" {
		t.Errorf("stamped version ignored: %s", Version())
	}
}

// Every build stamps BuildVersion through the same -X flag: the Makefile,
// goreleaser and the operator image. A build that stamps another symbol
// (the retired internal/cli.version) ships `lo version` as the embedded
// VERSION with no release stamp and nothing fails.
func TestBuildVersionFlagIsStampedByEveryBuild(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	const flag = "-X github.com/kernpilot/lok8s/internal/assets.BuildVersion="
	for _, rel := range []string{"Makefile", ".goreleaser.yaml", "operator/Dockerfile"} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), flag) {
			t.Errorf("%s does not stamp BuildVersion (want %q)", rel, flag)
		}
		if strings.Contains(string(raw), "internal/cli.version=") {
			t.Errorf("%s stamps the retired internal/cli.version symbol", rel)
		}
	}
}
