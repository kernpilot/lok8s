package kubeone

// robotcred_test.go: the KubeOne driver refuses a Robot credential
// (HROBOT_USER, HROBOT_PASSWORD) with a control character before any step
// that gets it. Provision and Destroy check first, and Apply and Reset
// check again for a direct caller. The steps: the infrastructure (the
// provider), the manifest render, the pre-apply steps (the bridge child
// that names the Robot servers and renders the CCM Secret), the run copy
// of the manifest, kubeone itself (robotEnv), and the removal of the
// kubeconfig. The refusal is the one of http::credential_ok
// (utils/http.sh), which the bash twin prints in
// kubeone::_name_robot_workers.
//
// A file with CRLF line ends can leave a CR at the end of a value. So the
// rows run with a CR at the end of the value, and with an LF, a tab or a
// DEL in it. The subtests are named by a label, never by a value.

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/provision"
	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

const robotDomain = "test.lok8s.dev"

// The fixtures are obvious fakes. robotValid holds the valid value of each
// name; robotForms makes the bad forms of a valid value, each with its
// label.
var (
	robotValid = map[string]string{
		"HROBOT_USER":     "robot-user-not-real",
		"HROBOT_PASSWORD": "robot-password-not-real",
	}
	robotOther = map[string]string{"HROBOT_USER": "HROBOT_PASSWORD", "HROBOT_PASSWORD": "HROBOT_USER"}
	robotForms = []struct {
		label string
		value func(valid string) string
	}{
		{"CR", func(v string) string { return v + "\r" }},
		{"LF", func(v string) string { return strings.Replace(v, "-not-", "-not\n", 1) }},
		{"TAB", func(v string) string { return strings.Replace(v, "-not-", "-not\t", 1) }},
		{"DEL", func(v string) string { return v + "\x7f" }},
	}
)

// robotRefusal is the refusal of http::credential_ok for name, as the bash
// twin prints it on a stderr that is not a terminal.
func robotRefusal(name string) string {
	return "[error] environment variable " + name + " must not contain a control character\n" +
		"  A file with CRLF line ends can leave a CR at the end of the value. Remove it, then try again.\n"
}

// robotError is the error that a refusal of names returns.
func robotError(names ...string) string {
	return "credentials: " + strings.Join(names, ", ") + " must not contain a control character"
}

// robotLeak reports whether s holds a part of a Robot fixture value.
func robotLeak(s string) bool {
	return strings.Contains(s, "robot-user") || strings.Contains(s, "robot-password")
}

// robotCmdLeak reports whether a recorded command carries a Robot value
// (argv, the extra environment, the directory).
func robotCmdLeak(c execx.Cmd) bool {
	return robotLeak(strings.Join(c.Args, " ")) || robotLeak(strings.Join(c.Env, " ")) || robotLeak(c.Dir)
}

// workTree is each file under root with its content.
func workTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// robotRig is a driver with a provider, a work dir that holds a manifest
// and a tfjson, the core template (so that a Provision that goes on
// renders the manifest), and the kubeconfig that a Destroy removes.
type robotRig struct {
	d         *Driver
	runner    *fakeRunner
	errBuf    *bytes.Buffer
	p         *config.Paths
	prov      *fakeProvider
	wd        string
	kc        string
	prepared  int
	runCopies int
}

func newRobotRig(t *testing.T) *robotRig {
	t.Helper()
	r := &robotRig{prov: &fakeProvider{}}
	r.d, r.runner, r.errBuf, r.p = testDriver(t, r.prov)
	r.d.Hooks.PrepareApply = func(ctx context.Context, workDir, clusterYAML string) error {
		r.prepared++
		return nil
	}
	// stageRun makes the run copy through mkdirTemp, and the deferred
	// cleanup removes it when Apply returns. So the run copy is counted
	// here, when it is made, not looked for in the files afterwards.
	orig := mkdirTemp
	mkdirTemp = func(dir, pattern string) (string, error) {
		r.runCopies++
		return orig(dir, pattern)
	}
	t.Cleanup(func() { mkdirTemp = orig })
	testutil.WriteFile(t, filepath.Join(r.p.Lok8s, "drivers", "kubeone", "cluster", "core", "kubeone.yaml"), realCoreTemplate(t))
	r.wd = filepath.Join(r.p.Clusters, robotDomain, ".kubeone")
	testutil.WriteFile(t, filepath.Join(r.wd, "kubeone.yaml"), "name: test-prod\n")
	testutil.WriteFile(t, filepath.Join(r.wd, "output.json"), "{}")
	r.kc = filepath.Join(r.p.Base, ".kubeconfig", "test-prod.yaml")
	testutil.WriteFile(t, r.kc, "apiVersion: v1\n")
	// kubeone apply writes <name>-kubeconfig into its CWD.
	r.runner.handler = func(c execx.Cmd) error {
		if len(c.Args) > 0 && c.Args[0] == "apply" {
			testutil.WriteFile(t, filepath.Join(c.Dir, "test-prod-kubeconfig"), "apiVersion: v1\nkind: Config\n")
		}
		return nil
	}
	return r
}

// assertRefused checks a refusal of names: the handled error with its
// exact text, the two lines of each name and nothing else on stderr, no
// value in either, and no step: no command, no pre-apply step, no run
// copy, no provider call, and the kubeconfig kept.
func (r *robotRig) assertRefused(t *testing.T, err error, names ...string) {
	t.Helper()
	if !errors.Is(err, ui.ErrHandled) {
		t.Errorf("error %v, want ErrHandled (exit status 1)", err)
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	if want := robotError(names...); errText != want {
		t.Errorf("error %q, want %q", errText, want)
	}
	want := ""
	for _, name := range names {
		want += robotRefusal(name)
	}
	if got := r.errBuf.String(); got != want {
		t.Errorf("stderr\n%q\nwant\n%q", got, want)
	}
	if robotLeak(r.errBuf.String()) || robotLeak(errText) {
		t.Errorf("the output holds the value: %q, %q", r.errBuf.String(), errText)
	}
	for _, c := range r.runner.calls {
		if robotCmdLeak(c) {
			t.Errorf("a command carries the value: %s %q", c.Name, c.Args)
		}
	}
	if len(r.runner.calls) != 0 {
		t.Errorf("a command ran: %d calls, first %s %q", len(r.runner.calls), r.runner.calls[0].Name, r.runner.calls[0].Args)
	}
	if r.prepared != 0 {
		t.Errorf("the pre-apply steps ran %d times", r.prepared)
	}
	if r.runCopies != 0 {
		t.Errorf("the run copy was made %d times", r.runCopies)
	}
	if len(r.prov.log) != 0 {
		t.Errorf("the provider ran: %q", r.prov.log)
	}
	if b, err := os.ReadFile(r.kc); err != nil || string(b) != "apiVersion: v1\n" {
		t.Errorf("the kubeconfig is not kept: %q, %v", b, err)
	}
}

// robotEntryPoints are the entry points that pass the Robot credentials
// on, each with the kubeone verb that runs when the values pass.
var robotEntryPoints = []struct {
	name string
	call func(ctx context.Context, d *Driver, workDir string) error
	verb string
}{
	{"Provision", func(ctx context.Context, d *Driver, _ string) error { return d.Provision(ctx, robotDomain) }, "apply"},
	{"Destroy", func(ctx context.Context, d *Driver, _ string) error { return d.Destroy(ctx, robotDomain) }, "reset"},
	{"Apply", func(ctx context.Context, d *Driver, workDir string) error { return d.Apply(ctx, workDir, "") }, "apply"},
	{"Reset", func(ctx context.Context, d *Driver, workDir string) error { return d.Reset(ctx, workDir) }, "reset"},
}

// Each row: one entry point, one name with a bad value, the other name
// valid or unset. The bash twin checks only when both are set; here each set
// value is checked, because robotEnv passes each one on alone. The files
// stay as they were: no rendered manifest, no removed tfjson or
// kubeconfig, no kubeconfig from kubeone.
func TestRobotCredentialWithAControlCharacterIsRefused(t *testing.T) {
	for _, ep := range robotEntryPoints {
		for _, name := range []string{"HROBOT_USER", "HROBOT_PASSWORD"} {
			for _, form := range robotForms {
				for _, otherSet := range []bool{true, false} {
					label := ep.name + "/" + name + "/" + form.label
					if !otherSet {
						label += "/other-unset"
					}
					t.Run(label, func(t *testing.T) {
						clearVarEnv(t)
						t.Setenv(name, form.value(robotValid[name]))
						if otherSet {
							t.Setenv(robotOther[name], robotValid[robotOther[name]])
						}
						r := newRobotRig(t)
						before := workTree(t, r.p.Base)

						err := ep.call(t.Context(), r.d, r.wd)
						r.assertRefused(t, err, name)
						after := workTree(t, r.p.Base)
						if len(after) != len(before) {
							t.Errorf("the refusal changed the files: %d before, %d after", len(before), len(after))
						}
						for path, content := range before {
							if after[path] != content {
								t.Errorf("the refusal changed %s", path)
							}
						}
					})
				}
			}
		}
	}
}

// Both names bad: each gets its two lines, in the order of http::credential_ok,
// and the error names both.
func TestRobotCredentialsBothRefusedInOrder(t *testing.T) {
	for _, ep := range robotEntryPoints {
		t.Run(ep.name, func(t *testing.T) {
			clearVarEnv(t)
			t.Setenv("HROBOT_USER", robotForms[0].value(robotValid["HROBOT_USER"]))
			t.Setenv("HROBOT_PASSWORD", robotForms[2].value(robotValid["HROBOT_PASSWORD"]))
			r := newRobotRig(t)
			err := ep.call(t.Context(), r.d, r.wd)
			r.assertRefused(t, err, "HROBOT_USER", "HROBOT_PASSWORD")
		})
	}
}

// Valid values pass, both or one alone, and so do unset ones: each entry
// point runs its steps, and kubeone gets each set value under its aliases.
// Apply and Provision make one run copy here, so the run-copy count that a
// refusal must keep at 0 can also be 1.
func TestRobotCredentialValidOrUnsetPasses(t *testing.T) {
	sets := []struct {
		label string
		names []string
	}{
		{"both", []string{"HROBOT_USER", "HROBOT_PASSWORD"}},
		{"user-only", []string{"HROBOT_USER"}},
		{"password-only", []string{"HROBOT_PASSWORD"}},
		{"unset", nil},
	}
	for _, ep := range robotEntryPoints {
		for _, set := range sets {
			t.Run(ep.name+"/"+set.label, func(t *testing.T) {
				clearVarEnv(t)
				var want []string
				for _, name := range set.names {
					t.Setenv(name, robotValid[name])
					alias := strings.TrimPrefix(name, "H")
					want = append(want, alias+"="+robotValid[name], "HETZNER_"+alias+"="+robotValid[name])
				}
				r := newRobotRig(t)
				if err := ep.call(t.Context(), r.d, r.wd); err != nil {
					t.Fatalf("%v\n%s", err, r.errBuf.String())
				}
				if r.errBuf.Len() != 0 {
					t.Errorf("stderr %q", r.errBuf.String())
				}
				if len(r.runner.calls) != 1 || r.runner.calls[0].Name != "kubeone" || r.runner.calls[0].Args[0] != ep.verb {
					t.Fatalf("calls %+v, want one kubeone %s", r.runner.calls, ep.verb)
				}
				if env := r.runner.calls[0].Env; !slices.Equal(env, want) {
					t.Errorf("kubeone env %q, want %q", env, want)
				}
				applies := map[bool]int{true: 1, false: 0}[ep.verb == "apply"]
				if r.prepared != applies || r.runCopies != applies {
					t.Errorf("pre-apply steps %d, run copies %d, want %d each", r.prepared, r.runCopies, applies)
				}
				wantLog := map[string][]string{"Provision": {"provision:" + r.wd}, "Destroy": {"destroy:" + r.wd}}[ep.name]
				if !slices.Equal(r.prov.log, wantLog) {
					t.Errorf("provider log %q, want %q", r.prov.log, wantLog)
				}
				if _, err := os.Stat(r.kc); (err == nil) == (ep.name == "Destroy") {
					t.Errorf("kubeconfig after %s: %v", ep.name, err)
				}
			})
		}
	}
}

// Destroy refuses before the reset, as before every other step: a failed
// reset only warns, and the provider destroy that would follow would
// delete the cloud servers while the Robot servers keep their Kubernetes
// install. So there is no warning, no kubeone, no provider destroy, and
// the kubeconfig stays.
func TestDestroyRefusesARobotCredentialBeforeTheReset(t *testing.T) {
	clearVarEnv(t)
	t.Setenv("HROBOT_USER", robotValid["HROBOT_USER"])
	t.Setenv("HROBOT_PASSWORD", robotForms[0].value(robotValid["HROBOT_PASSWORD"]))
	r := newRobotRig(t)
	err := r.d.Destroy(t.Context(), robotDomain)
	r.assertRefused(t, err, "HROBOT_PASSWORD")
	if _, statErr := os.Stat(filepath.Join(r.wd, "kubeone.yaml")); statErr != nil {
		t.Errorf("the manifest is gone: %v", statErr)
	}
}

// Provision refuses before the provider: a first provision of a domain
// with a bad value makes no server, renders no manifest and runs nothing.
// The work dir stays empty.
func TestProvisionRefusesARobotCredentialBeforeTheProvider(t *testing.T) {
	clearVarEnv(t)
	t.Setenv("HROBOT_USER", robotForms[0].value(robotValid["HROBOT_USER"]))
	t.Setenv("HROBOT_PASSWORD", robotValid["HROBOT_PASSWORD"])
	r := newRobotRig(t)
	if err := os.RemoveAll(r.wd); err != nil {
		t.Fatal(err)
	}
	err := r.d.Provision(t.Context(), robotDomain)
	r.assertRefused(t, err, "HROBOT_USER")
	entries, readErr := os.ReadDir(r.wd)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Errorf("the work dir holds %d entries, first %s", len(entries), entries[0].Name())
	}
}

// The common cause: a store entry with CRLF line ends. LoadProviderCreds
// keeps the CR, as bash $(cat …) does, and each entry point refuses it
// before any step.
func TestRobotCredentialFromACRLFStoreIsRefused(t *testing.T) {
	for _, ep := range robotEntryPoints {
		for _, name := range []string{"HROBOT_USER", "HROBOT_PASSWORD"} {
			t.Run(ep.name+"/"+name, func(t *testing.T) {
				clearVarEnv(t)
				r := newRobotRig(t)
				secrets := filepath.Join(r.p.Clusters, robotDomain, "secrets")
				testutil.WriteFile(t, filepath.Join(secrets, "Secret.hetzner.provisioning."+name), robotValid[name]+"\r\n")
				other := robotOther[name]
				testutil.WriteFile(t, filepath.Join(secrets, "Secret.hetzner.provisioning."+other), robotValid[other]+"\n")
				if err := provision.LoadProviderCreds(r.p, robotDomain); err != nil {
					t.Fatal(err)
				}
				if got := os.Getenv(name); got != robotValid[name]+"\r" {
					t.Fatalf("%s holds %d bytes, want the value with its CR", name, len(got))
				}
				err := ep.call(t.Context(), r.d, r.wd)
				r.assertRefused(t, err, name)
			})
		}
	}
}
