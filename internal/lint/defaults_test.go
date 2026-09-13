package lint

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

const loSpecWithDefaults = `apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: demo
spec:
  cluster:
    domain: demo.dev
  registries:
    mirrors:
      - name: io-docker
        url: https://registry-1.docker.io
      - name: io-quay
        url: https://quay.io
      - name: io-k8s
        url: https://registry.k8s.io
      - name: io-ghcr
        url: https://ghcr.io
  nodes:
    controlPlane: 1
    workers: 2
  runtime: kind
  bootstrap:
    - cilium
`

// --notes prints one line per key equal to its documented default, on
// stdout, with the spec path relative to the project; without --notes
// nothing (the parity contract: the bash lint has no such line).
func TestNotesDefaultEqualKeys(t *testing.T) {
	t.Parallel()
	l, base, out, errOut := newLinter(t)
	spec := filepath.Join(base, "clusters", "demo.dev", "cluster.lok8s.yaml")
	testutil.WriteFile(t, spec, loSpecWithDefaults)

	l.notes(spec)
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("without Notes something printed:\n%s%s", out.String(), errOut.String())
	}
	l.Notes = true
	l.notes(spec)
	want := "[note] clusters/demo.dev/cluster.lok8s.yaml: spec.nodes.controlPlane equals the default (1); you can drop it\n" +
		"[note] clusters/demo.dev/cluster.lok8s.yaml: spec.runtime equals the default (kind); you can drop it\n" +
		"[note] clusters/demo.dev/cluster.lok8s.yaml: spec.registries.mirrors equals the default (io-docker, io-quay, io-k8s, io-ghcr on the standard upstream URLs); you can drop it\n"
	if out.String() != want {
		t.Errorf("notes:\n%s\nwant:\n%s", out.String(), want)
	}
	if errOut.Len() != 0 {
		t.Errorf("notes on stderr: %s", errOut.String())
	}
}

// Keys that differ from the default, a partial or extended mirror list,
// a spec of another driver: no note.
func TestNotesSilentWhenNotDefault(t *testing.T) {
	t.Parallel()
	mirrors := func(extra string) string {
		return "kind: Lo\nspec:\n  registries:\n    mirrors:\n" +
			"      - name: io-docker\n        url: https://registry-1.docker.io\n" +
			"      - name: io-quay\n        url: https://quay.io\n" +
			"      - name: io-k8s\n        url: https://registry.k8s.io\n" +
			"      - name: io-ghcr\n        url: https://ghcr.io\n" + extra
	}
	cases := map[string]string{
		"other values":     "kind: Lo\nspec:\n  nodes:\n    controlPlane: 3\n  runtime: k3s\n",
		"absent keys":      "apiVersion: v1\nkind: Lo\nmetadata:\n  name: x\nspec:\n  cluster:\n    domain: x.dev\n",
		"null keys":        "kind: Lo\nspec:\n  nodes:\n    controlPlane: ~\n  runtime: null\n  registries:\n    mirrors: []\n",
		"one mirror":       "kind: Lo\nspec:\n  registries:\n    mirrors:\n      - name: io-docker\n        url: https://registry-1.docker.io\n",
		"extra mirror":     mirrors("      - name: mine\n        url: https://r.example\n"),
		"mirror url":       strings.Replace(mirrors(""), "https://quay.io", "https://mirror.example/quay", 1),
		"mirror extra key": strings.Replace(mirrors(""), "        url: https://quay.io\n", "        url: https://quay.io\n        insecure: true\n", 1),
		"duplicate mirror": strings.Replace(mirrors(""), "      - name: io-ghcr\n        url: https://ghcr.io\n", "      - name: io-quay\n        url: https://quay.io\n", 1),
		"kubeone spec":     strings.Replace(loSpecWithDefaults, "kind: Lo", "kind: KubeOne", 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			l, base, out, _ := newLinter(t)
			spec := filepath.Join(base, "clusters", "x.dev", "cluster.lok8s.yaml")
			testutil.WriteFile(t, spec, body)
			l.Notes = true
			l.notes(spec)
			if out.Len() != 0 {
				t.Errorf("unexpected note:\n%s", out.String())
			}
		})
	}
}

// The notes ride Run: they print inside the domain block on stdout and
// never change the verdict or the exit code (the same project with and
// without Notes yields the same error).
func TestNotesDoNotChangeTheVerdict(t *testing.T) {
	t.Parallel()
	run := func(notes bool) (string, error) {
		l, base, out, _ := newLinter(t)
		testutil.WriteFile(t, filepath.Join(base, "clusters", "demo.dev", "cluster.lok8s.yaml"), loSpecWithDefaults)
		testutil.WriteFile(t, filepath.Join(base, "services.yaml"), "services: {}\n")
		l.Notes = notes
		err := l.Run("demo.dev")
		return out.String(), err
	}
	with, errWith := run(true)
	without, errWithout := run(false)
	if (errWith == nil) != (errWithout == nil) {
		t.Errorf("notes changed the verdict: with=%v without=%v", errWith, errWithout)
	}
	line := "[note] clusters/demo.dev/cluster.lok8s.yaml: spec.runtime equals the default (kind); you can drop it\n"
	if !strings.Contains(with, line) {
		t.Errorf("no note through Run:\n%s", with)
	}
	if strings.Contains(without, "[note]") {
		t.Errorf("a note without Notes:\n%s", without)
	}
	if strings.Replace(with, line, "", 1) == with || !strings.Contains(with, "Validating: demo.dev\n") {
		t.Errorf("unexpected shape:\n%s", with)
	}
}
