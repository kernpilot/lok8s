package lint

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// newLinter builds a Linter over a synthetic project root with capture
// buffers for both streams.
func newLinter(t *testing.T) (*Linter, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "clusters"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	l := &Linter{
		Paths: &config.Paths{
			Base:     base,
			Bin:      filepath.Join(base, ".bin"),
			Lok8s:    filepath.Join(base, ".lok8s"),
			Clusters: filepath.Join(base, "clusters"),
		},
		Out:    out,
		ErrOut: errOut,
	}
	return l, base, out, errOut
}

func TestSchemaMissingFields(t *testing.T) {
	t.Parallel()
	l, base, _, errOut := newLinter(t)
	dir := filepath.Join(base, "clusters", "a.dev")
	testutil.WriteFile(t, filepath.Join(dir, "cluster.lok8s.yaml"), "spec: {}\n")

	if got := l.schema(dir, filepath.Join(dir, "cluster.lok8s.yaml")); got != 3 {
		t.Fatalf("schema errors = %d, want 3", got)
	}
	for _, want := range []string{
		"  Missing required field: kind",
		"  Missing required field: apiVersion",
		"  Missing required field: metadata.name",
		"  Missing spec.kind (cluster runtime type)",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr missing %q; got:\n%s", want, errOut.String())
		}
	}
}

func TestApexSubdomainViolation(t *testing.T) {
	t.Parallel()
	l, base, _, errOut := newLinter(t)
	testutil.WriteFile(t, filepath.Join(base, "clusters", "apex.dev", "cluster.lok8s.yaml"), "kind: Lo\n")
	testutil.WriteFile(t, filepath.Join(base, "clusters", "sub.apex.dev", "cluster.lok8s.yaml"), "kind: Lo\n")

	if l.apex() {
		t.Fatal("apex() = ok, want violation")
	}
	want := "cluster 'sub.apex.dev' is a subdomain of cluster 'apex.dev'"
	if !strings.Contains(errOut.String(), want) {
		t.Errorf("stderr missing %q; got:\n%s", want, errOut.String())
	}
}

func TestLabelsQueryMultiDocQuirk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Single unlabelled doc → "0" → warns.
	single := filepath.Join(dir, "single.yaml")
	testutil.WriteFile(t, single, "metadata:\n  name: x\n")
	if got := labelsQuery(single); got != "0" {
		t.Fatalf("labelsQuery(single) = %q, want \"0\"", got)
	}

	// Multi-doc whose FIRST doc carries the label: bash captures "1\n0"
	// (yq prints 1, then errors on doc2, then `|| echo 0`) — NOT "0", so no
	// warning. The quirk is the contract.
	multi := filepath.Join(dir, "multi.yaml")
	testutil.WriteFile(t, multi, "metadata:\n  labels:\n    lok8s.dev/name: x\n---\nkind: Foo\n")
	if got := labelsQuery(multi); got != "1\n0" {
		t.Fatalf("labelsQuery(multi) = %q, want \"1\\n0\"", got)
	}

	// Labelled single doc → "1".
	labelled := filepath.Join(dir, "labelled.yaml")
	testutil.WriteFile(t, labelled, "metadata:\n  labels:\n    lok8s.dev/name: x\n")
	if got := labelsQuery(labelled); got != "1" {
		t.Fatalf("labelsQuery(labelled) = %q, want \"1\"", got)
	}
}

// TestKubernetesVersion: `lo lint` reports the spec.kubernetes.version that
// `lo provision` refuses (lodriver.NodeVersion), on a Lo cluster spec only.
// An unset version is clean: kind uses its default node image.
func TestKubernetesVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, spec, wantErr string
	}{
		{name: "lo, no version", spec: "kind: Lo\nspec: {}\n"},
		{name: "lo, literal null", spec: "kind: Lo\nspec:\n  kubernetes:\n    version: \"null\"\n"},
		{name: "lo, valid", spec: "kind: Lo\nspec:\n  kubernetes:\n    version: v1.35.0\n"},
		{name: "lo, malformed", spec: "kind: Lo\nspec:\n  kubernetes:\n    version: \"1.31\"\n",
			wantErr: "  spec.kubernetes.version '1.31' is not a kind node version. Use the form vMAJOR.MINOR.PATCH, with an optional @sha256:<digest>, or remove the key to use the default node image of kind."},
		{name: "lo, map", spec: "kind: Lo\nspec:\n  kubernetes:\n    version: {minor: 31}\n",
			wantErr: "  spec.kubernetes.version is a map, not a kind node version."},
		// Other drivers name the version in their own way (KubeOne: 1.31.0).
		{name: "kubeone, no v prefix", spec: "kind: KubeOne\nspec:\n  kubernetes:\n    version: \"1.31.0\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, base, _, errOut := newLinter(t)
			dir := filepath.Join(base, "clusters", "a.dev")
			spec := filepath.Join(dir, "cluster.lok8s.yaml")
			testutil.WriteFile(t, spec, tc.spec)

			got := l.kubernetesVersion(dir, spec)
			if tc.wantErr == "" {
				if got != 0 || errOut.Len() != 0 {
					t.Fatalf("kubernetesVersion = %d, want 0 and no output; stderr:\n%s", got, errOut.String())
				}
				return
			}
			if got != 1 || !strings.Contains(errOut.String(), tc.wantErr) {
				t.Fatalf("kubernetesVersion = %d, want 1 and %q; stderr:\n%s", got, tc.wantErr, errOut.String())
			}
		})
	}
}

// TestRunReportsMalformedKubernetesVersion: the check is wired into the
// per-domain run and decides its verdict.
func TestRunReportsMalformedKubernetesVersion(t *testing.T) {
	t.Parallel()
	l, base, out, errOut := newLinter(t)
	testutil.WriteFile(t, filepath.Join(base, "clusters", "a.dev", "cluster.lok8s.yaml"),
		"apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: a\nspec:\n  kubernetes:\n    version: \"1.31\"\n  bootstrap: []\n")
	if err := l.Run("a.dev"); err == nil {
		t.Fatalf("lint passed a malformed version; stdout:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "spec.kubernetes.version '1.31' is not a kind node version.") {
		t.Fatalf("the version problem is not reported; stderr:\n%s", errOut.String())
	}
	if strings.Contains(out.String(), "  OK") {
		t.Fatalf("a failing domain printed OK:\n%s", out.String())
	}
}
