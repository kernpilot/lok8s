package cli

// cmd_bootstrap_dryrun_test.go — `lo bootstrap --dry-run` and `lo provision
// --bootstrap --dry-run` through the REAL command dispatch (the cobra tree,
// the bootstrap/provision dispatchers, the Lo driver's Export, the engine,
// the addon render), with only the exec seam faked: kustomize answers a
// canned manifest, kubectl answers the diff. Review round 3 on #268.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/testutil"
)

const dryDomain = "dry.lok8s.dev"

// dryProject is orchestrateProject plus a Lo domain whose one bootstrap
// entry is a local target, its kubeconfig, and a project tree a whole-tree
// walk could not survive: a dangling symlink, a directory nobody may read
// and an 8 GiB sparse file under an ignored cache directory.
func dryProject(t *testing.T, entry string) (*config.Paths, *scriptRunner, *[]int) {
	t.Helper()
	p, r, exits := orchestrateProject(t)
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	t.Setenv("PATH_SECRETS", "")
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "")
	os.Unsetenv("KUSTOMIZE_PLUGIN_HOME")
	testutil.WriteFile(t, filepath.Join(p.Base, ".kubeconfig", "dryc.yaml"), "")
	testutil.WriteFile(t, filepath.Join(p.Clusters, dryDomain, "cluster.lok8s.yaml"),
		"kind: Lo\nmetadata:\n  name: dryc\nspec:\n  runtime: kind\n  network:\n    name: drynet\n    cidr: 10.99.12.0/24\n  registries:\n    tls: false\n  bootstrap:\n    - "+entry+"\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, dryDomain, "targets", "app", "kustomization.yaml"), "resources: []\n")

	if err := os.Symlink("/nonexistent/lok8s-dry-run-target", filepath.Join(p.Base, "dangling")); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(p.Base, "locked")
	testutil.WriteFile(t, filepath.Join(locked, "secret"), "x\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	huge := filepath.Join(p.Base, ".cache", "core")
	if err := os.MkdirAll(filepath.Dir(huge), 0o755); err != nil {
		t.Fatal(err)
	}
	fh, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	if err := fh.Truncate(8 << 30); err != nil {
		t.Fatal(err)
	}
	_ = fh.Close()
	return p, r, exits
}

// treeState lists every path under root with its mode, size and mtime,
// without reading file contents (the sparse file) and without entering a
// directory it cannot read.
func treeState(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(&b, "%s unreadable\n", path)
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil
		}
		fmt.Fprintf(&b, "%s %v %d %d\n", path, info.Mode(), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]) + "\n" + b.String()
}

// diffHandler answers the dry run's exec calls: kustomize renders one
// ConfigMap (and records what the render saw), kubectl only ever diffs.
func diffHandler(t *testing.T, rc int, seen map[string]string) func(c execx.Cmd) error {
	return func(c execx.Cmd) error {
		switch c.Name {
		case "kustomize":
			for _, k := range []string{"LOK8S_SPEC_REGISTRY_BUILD_HOST", "LOK8S_SPEC_REGISTRY_CACHE_HOST", "LOK8S_REGISTRY_IP_BUILD", "LOK8S_BOOTSTRAP_DRYRUN"} {
				seen[k] = os.Getenv(k)
			}
			fmt.Fprint(c.Stdout, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n")
			return nil
		case "kubectl":
			if len(c.Args) < 3 || c.Args[2] != "diff" {
				t.Errorf("a dry run reached kubectl %v", c.Args)
				return errors.New("not a diff")
			}
			if rc == 0 {
				return nil
			}
			fmt.Fprint(c.Stdout, "--- a\n+++ b\n")
			return &exitCodeError{rc}
		}
		t.Errorf("unexpected exec: %s %v", c.Name, c.Args)
		return errors.New("unexpected exec")
	}
}

type exitCodeError struct{ code int }

func (e *exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *exitCodeError) ExitCode() int { return e.code }

// The dry run renders in place: it never walks the project (a dangling
// symlink, an unreadable directory, an 8 GiB sparse file are all left
// alone), writes nothing into it (no .registries.json, no ejected addon),
// and still exports the registry values a real run reads back from
// .registries.json — on both command boundaries.
func TestDryRunThroughTheDispatchNeverWalksOrWritesTheProject(t *testing.T) {
	for _, argv := range [][]string{
		{"bootstrap", "--dry-run", "--domain", dryDomain},
		{"provision", "--bootstrap", "--dry-run", "--domain", dryDomain},
	} {
		t.Run(argv[0], func(t *testing.T) {
			p, r, exits := dryProject(t, "./targets/app")
			seen := map[string]string{}
			r.handler = diffHandler(t, 0, seen)
			before := treeState(t, p.Base)
			stdout, stderr, err := runLo(t, NewRoot(p), argv...)
			if err != nil || len(*exits) != 0 {
				t.Fatalf("err=%v exits=%v\n%s\n%s", err, *exits, stdout, stderr)
			}
			if !strings.Contains(stdout, "bootstrap dry-run: app: unchanged\n") {
				t.Errorf("stdout:\n%s", stdout)
			}
			if after := treeState(t, p.Base); after != before {
				t.Errorf("the dry run changed the project tree:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			if _, err := os.Stat(filepath.Join(p.Clusters, dryDomain, ".registries.json")); err == nil {
				t.Error("the dry run wrote .registries.json")
			}
			// The values a real run reads back from the file it writes.
			if seen["LOK8S_SPEC_REGISTRY_BUILD_HOST"] != "lok8s.local" || seen["LOK8S_SPEC_REGISTRY_CACHE_HOST"] != "lok8s.cache" || seen["LOK8S_REGISTRY_IP_BUILD"] == "" {
				t.Errorf("registry values the render saw: %v", seen)
			}
			if seen["LOK8S_BOOTSTRAP_DRYRUN"] != "1" {
				t.Errorf("the render ran without the dry-run sentinel: %v", seen)
			}
		})
	}
}

// An entry's env: may not set lo's own variables: an entry error on the
// dry run (exit 2) and on the real run (exit 1), and the render never runs.
func TestEntryEnvReservedNameThroughTheDispatch(t *testing.T) {
	for _, dry := range []bool{true, false} {
		p, r, exits := dryProject(t, "./targets/app: {env: {KUSTOMIZE_PLUGIN_HOME: /tmp/evil}}")
		r.handler = func(c execx.Cmd) error {
			t.Errorf("dry=%v: exec despite the refusal: %s %v", dry, c.Name, c.Args)
			return errors.New("no exec")
		}
		argv := []string{"bootstrap", "--domain", dryDomain}
		if dry {
			argv = append(argv, "--dry-run")
		}
		stdout, stderr, _ := runLo(t, NewRoot(p), argv...)
		if !strings.Contains(stdout+stderr, "bootstrap: app: env: may not set KUSTOMIZE_PLUGIN_HOME (reserved for lo)") {
			t.Errorf("dry=%v: output:\n%s\n%s", dry, stdout, stderr)
		}
		if dry && (len(*exits) != 1 || (*exits)[0] != 2) {
			t.Errorf("dry run: exits=%v, want [2]", *exits)
		}
		if !dry && len(*exits) != 0 {
			t.Errorf("real run: exits=%v, want rc 1 (no explicit exit)", *exits)
		}
	}
}

// Exit codes at both boundaries: 1 ONLY for a diff that found a change,
// 2 for every other failure of a dry run (before or outside the per-entry
// diff), and the real run's codes unchanged.
func TestDryRunExitCodesAtBothBoundaries(t *testing.T) {
	type tc struct {
		name  string
		argv  []string
		rc    int // the diff's rc
		exits []int
	}
	for _, c := range []tc{
		{"bootstrap differs", []string{"bootstrap", "--dry-run", "--domain", dryDomain}, 1, nil},
		{"provision differs", []string{"provision", "-b", "--dry-run", "--domain", dryDomain}, 1, nil},
		{"bootstrap diff error", []string{"bootstrap", "--dry-run", "--domain", dryDomain}, 3, []int{2}},
		{"bootstrap missing spec", []string{"bootstrap", "--dry-run", "--domain", "missing.dev"}, 0, []int{2}},
		{"provision missing spec", []string{"provision", "-b", "--dry-run", "--domain", "missing.dev"}, 0, []int{2}},
		{"bootstrap blank --only", []string{"bootstrap", "--dry-run", "--only", "", "--domain", dryDomain}, 0, []int{2}},
		{"bootstrap unknown --only", []string{"bootstrap", "--dry-run", "--only", "nope", "--domain", dryDomain}, 0, []int{2}},
		{"provision without --bootstrap", []string{"provision", "--dry-run", "--domain", dryDomain}, 0, []int{2}},
		{"bootstrap positional", []string{"bootstrap", "--dry-run", "extra"}, 0, []int{2}},
		{"bootstrap unknown flag", []string{"bootstrap", "--bogus", "--dry-run"}, 0, []int{2}},
		{"provision unknown flag", []string{"provision", "-b", "--dry-run", "--bogus"}, 0, []int{2}},
		{"provision --ignore-nondeterministic without --bootstrap", []string{"provision", "--dry-run", "--ignore-nondeterministic", "--domain", dryDomain}, 0, []int{2}},
		{"bootstrap --dry-run --only= (blank)", []string{"bootstrap", "--dry-run", "--only=", "--domain", dryDomain}, 0, []int{2}},
		{"bootstrap --dry-run --with-restore", []string{"bootstrap", "--dry-run", "--with-restore", "--domain", dryDomain}, 0, []int{2}},
		// The real run keeps rc 1 for the same failures.
		{"real --ignore-nondeterministic", []string{"bootstrap", "--ignore-nondeterministic", "--domain", dryDomain}, 0, nil},
		{"real missing spec", []string{"bootstrap", "--domain", "missing.dev"}, 0, nil},
		{"real unknown flag", []string{"bootstrap", "--bogus"}, 0, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, r, exits := dryProject(t, "./targets/app")
			r.handler = diffHandler(t, c.rc, map[string]string{})
			prev := os.Args
			os.Args = append([]string{"lo"}, c.argv...)
			t.Cleanup(func() { os.Args = prev })
			_, stderr, err := runLo(t, NewRoot(p), c.argv...)
			if err == nil {
				t.Fatalf("no error (stderr %q)", stderr)
			}
			if fmt.Sprint(*exits) != fmt.Sprint(c.exits) {
				t.Errorf("exits=%v, want %v\nstderr:\n%s", *exits, c.exits, stderr)
			}
		})
	}
}
