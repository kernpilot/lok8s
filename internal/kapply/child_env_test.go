package kapply

// child_env_test.go — the environment of a real kubectl child (bats: "kuberc:
// a kubectl on PATH runs the apply, the heal's replace and the finalize
// with KUBERC=off" in tests/unit/kapply_test.bats). The real runner
// (execx.NewRunner) starts the test binary again as kubectl
// (TestKubectlHelperProcess): the child writes the KUBERC of its own
// environment and its argv to a file, so the test sees what kubectl gets,
// not the Env field of a Cmd.

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/clock"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

// kubectlHelperEnv names the directory of TestKubectlHelperProcess; only
// TestApplyChildrenGetKubeRCOffOnEachWrite sets it.
const kubectlHelperEnv = "LOK8S_TEST_KUBECTL_HELPER_DIR"

// helperHealErr is what the first apply of the helper prints on stderr: an
// immutable-field error of the manifest object and a namespace that is
// being terminated, so the pass heals both (the replace and the finalize).
const helperHealErr = `Error from server (Invalid): Deployment.apps "web" is invalid: spec.selector: field is immutable
Error from server (Forbidden): configmaps "cfg" is forbidden: unable to create new content in namespace demo because it is being terminated`

// TestKubectlHelperProcess is the kubectl of
// TestApplyChildrenGetKubeRCOffOnEachWrite: the test binary, started again
// by a stand-in script with the kubectl argv after "--". It appends one
// line to <dir>/calls: KUBERC=<the KUBERC of its environment, or <unset>>,
// a tab, its argv. It reads its stdin for -f -. The first apply prints
// helperHealErr on stderr and exits 1, a later one exits 0; the two reads
// of the namespace say that it is terminating; any other call exits 0.
func TestKubectlHelperProcess(t *testing.T) {
	dir := os.Getenv(kubectlHelperEnv)
	if dir == "" {
		t.Skip("the kubectl process of TestApplyChildrenGetKubeRCOffOnEachWrite")
	}
	args := os.Args
	if i := slices.Index(args, "--"); i >= 0 {
		args = args[i+1:]
	}
	os.Exit(helperKubectl(dir, args))
}

// helperKubectl is the body of TestKubectlHelperProcess; it returns the
// exit code (97 when it cannot write its log).
func helperKubectl(dir string, args []string) int {
	kuberc, ok := os.LookupEnv("KUBERC")
	if !ok {
		kuberc = "<unset>"
	}
	f, err := os.OpenFile(filepath.Join(dir, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 97
	}
	_, werr := f.WriteString("KUBERC=" + kuberc + "\t" + strings.Join(args, " ") + "\n")
	if cerr := f.Close(); werr != nil || cerr != nil {
		return 97
	}
	if strings.Contains(" "+strings.Join(args, " ")+" ", " -f - ") {
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	switch {
	case len(args) > 0 && args[0] == "apply":
		first := filepath.Join(dir, "applied")
		if _, err := os.Stat(first); err == nil {
			return 0
		}
		if os.WriteFile(first, nil, 0o600) != nil {
			return 97
		}
		fmt.Fprintln(os.Stderr, helperHealErr)
		return 1
	case slices.Equal(args, []string{"get", "ns", "demo", "-o", "jsonpath={.metadata.deletionTimestamp}"}):
		fmt.Println("2026-01-01T00:00:00Z")
	case slices.Equal(args, []string{"get", "ns", "demo", "-o", "json"}):
		fmt.Println(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"demo"},"spec":{"finalizers":["kubernetes"]}}`)
	}
	return 0
}

// An apply that heals (the replace of the immutable object and the
// finalize of the namespace), with the real runner and a KUBERC of the
// user set. Each child that writes from stdin (the two apply passes, the
// replace, the replace of the finalize) has KUBERC=off in its own
// environment: the runner appends the Env of the Cmd after the inherited
// KUBERC, and the last one wins. Each read keeps the KUBERC of the user.
func TestApplyChildrenGetKubeRCOffOnEachWrite(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	script := "#!/bin/sh\nexec " + quote(self) + " -test.run='^TestKubectlHelperProcess$' -test.count=1 -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o700); err != nil { // #nosec G306 -- a test executable
		t.Fatal(err)
	}
	t.Setenv(kubectlHelperEnv, dir)
	t.Setenv("KUBERC", "/user/kuberc")
	setUI(t, false, false)
	var out, errOut bytes.Buffer
	a := &Applier{
		Runner: execx.NewRunner(&config.Paths{Bin: bin}), Stdout: &out, Stderr: &errOut,
		NonInteractive: true, ForceRecreate: true,
		NsWait: 0, Sleep: clock.NoSleep, PollInterval: time.Nanosecond,
	}
	if _, rc := a.Apply(t.Context(), "resources", deployManifest); rc != 0 {
		t.Fatalf("rc = %d, want 0:\n%s", rc, errOut.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
		got[line]++
	}
	want := map[string]int{
		"KUBERC=off\tapply --server-side --force-conflicts -f -":                     2,
		"KUBERC=off\treplace --force -f -":                                           1,
		"KUBERC=off\treplace --raw /api/v1/namespaces/demo/finalize -f -":            1,
		"KUBERC=/user/kuberc\tget ns demo -o jsonpath={.metadata.deletionTimestamp}": 1,
		"KUBERC=/user/kuberc\tget ns demo -o json":                                   1,
		// the probe of the heal for a stuck-Terminating manifest object
		"KUBERC=/user/kuberc\tget Deployment web -n default -o jsonpath={.metadata.deletionTimestamp}": 1,
	}
	if !maps.Equal(got, want) {
		t.Errorf("the kubectl children (KUBERC, argv):\n%s\nwant, in any order: %v", raw, want)
	}
}
