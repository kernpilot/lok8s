package kubeone

// robotcred_test.go: Apply and Reset refuse a Robot credential
// (HROBOT_USER, HROBOT_PASSWORD) with a control character before any step
// that gets it: the pre-apply steps (the bridge child that names the Robot
// servers and renders the CCM Secret), the run copy of the manifest and
// kubeone itself (robotEnv). The refusal is the one of http::credential_ok
// (utils/http.sh), which the bash twin prints in kubeone::_name_robot_workers.
//
// A file with CRLF line ends can leave a CR at the end of a value. So each
// row runs with a CR at the end of the value, and with a tab in it.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/provision"
	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// The fixtures are obvious fakes. robotValid holds the valid value of each
// name; robotBad the values with a control character.
var (
	robotValid = map[string]string{
		"HROBOT_USER":     "robot-user-not-real",
		"HROBOT_PASSWORD": "robot-password-not-real",
	}
	robotBad = map[string][]string{
		"HROBOT_USER":     {"robot-user-not-real\r", "robot-user-not\treal"},
		"HROBOT_PASSWORD": {"robot-password-not-real\r", "robot-password-not\treal"},
	}
	robotOther = map[string]string{"HROBOT_USER": "HROBOT_PASSWORD", "HROBOT_PASSWORD": "HROBOT_USER"}
)

// robotRefusal is the refusal of http::credential_ok for name, as the bash
// twin prints it on a stderr that is not a terminal.
func robotRefusal(name string) string {
	return "[error] environment variable " + name + " must not contain a control character\n" +
		"  A file with CRLF line ends can leave a CR at the end of the value. Remove it, then try again.\n"
}

// robotLeak reports whether s holds a part of a Robot fixture value.
func robotLeak(s string) bool {
	for _, part := range []string{"robot-user", "robot-password", "not-real", "not\treal"} {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
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

// robotEntryPoints are the entry points that pass the Robot credentials on.
var robotEntryPoints = []struct {
	name string
	call func(ctx context.Context, d *Driver, workDir string) error
}{
	{"Apply", func(ctx context.Context, d *Driver, workDir string) error { return d.Apply(ctx, workDir, "") }},
	{"Reset", func(ctx context.Context, d *Driver, workDir string) error { return d.Reset(ctx, workDir) }},
}

// Each row: one entry point, one name with a bad value, the other name
// valid or unset. The bash twin checks only when both are set; here each set
// value is checked, because robotEnv passes each one on alone.
func TestRobotCredentialWithAControlCharacterIsRefused(t *testing.T) {
	for _, ep := range robotEntryPoints {
		for _, name := range []string{"HROBOT_USER", "HROBOT_PASSWORD"} {
			for _, value := range robotBad[name] {
				for _, otherSet := range []bool{true, false} {
					label := ep.name + "/" + name + "/" + strings.NewReplacer("\r", "CR", "\t", "TAB").Replace(value)
					if !otherSet {
						label += "/other-unset"
					}
					t.Run(label, func(t *testing.T) {
						clearVarEnv(t)
						t.Setenv(name, value)
						if otherSet {
							t.Setenv(robotOther[name], robotValid[robotOther[name]])
						}
						d, runner, errBuf, p := testDriver(t, nil)
						prepared := 0
						d.Hooks.PrepareApply = func(ctx context.Context, workDir, clusterYAML string) error {
							prepared++
							return nil
						}
						wd := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
						testutil.WriteFile(t, filepath.Join(wd, "kubeone.yaml"), "name: test-prod\n")
						testutil.WriteFile(t, filepath.Join(wd, "output.json"), "{}")
						before := workTree(t, p.Base)

						err := ep.call(t.Context(), d, wd)
						if !errors.Is(err, ui.ErrHandled) {
							t.Errorf("error %v, want ErrHandled (exit status 1)", err)
						}
						if got, want := errBuf.String(), robotRefusal(name); got != want {
							t.Errorf("stderr\n%q\nwant\n%q", got, want)
						}
						errText := ""
						if err != nil {
							errText = err.Error()
						}
						if robotLeak(errBuf.String()) || robotLeak(errText) {
							t.Errorf("the output holds the value: %q, %q", errBuf.String(), errText)
						}
						for _, c := range runner.calls {
							if robotCmdLeak(c) {
								t.Errorf("a command carries the value: %s %q", c.Name, c.Args)
							}
						}
						if len(runner.calls) != 0 {
							t.Errorf("a command ran: %d calls, first %s %q", len(runner.calls), runner.calls[0].Name, runner.calls[0].Args)
						}
						if prepared != 0 {
							t.Errorf("the pre-apply steps ran %d times", prepared)
						}
						after := workTree(t, p.Base)
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

// Both names bad: each gets its two lines, in the order of http::credential_ok.
func TestRobotCredentialsBothRefusedInOrder(t *testing.T) {
	for _, ep := range robotEntryPoints {
		t.Run(ep.name, func(t *testing.T) {
			clearVarEnv(t)
			t.Setenv("HROBOT_USER", robotBad["HROBOT_USER"][0])
			t.Setenv("HROBOT_PASSWORD", robotBad["HROBOT_PASSWORD"][1])
			d, runner, errBuf, p := testDriver(t, nil)
			wd := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
			testutil.WriteFile(t, filepath.Join(wd, "kubeone.yaml"), "name: test-prod\n")
			err := ep.call(t.Context(), d, wd)
			if !errors.Is(err, ui.ErrHandled) {
				t.Fatalf("error %v, want ErrHandled", err)
			}
			if got, want := errBuf.String(), robotRefusal("HROBOT_USER")+robotRefusal("HROBOT_PASSWORD"); got != want {
				t.Errorf("stderr\n%q\nwant\n%q", got, want)
			}
			if len(runner.calls) != 0 {
				t.Errorf("a command ran: %d calls", len(runner.calls))
			}
		})
	}
}

// Valid values, and unset or empty ones, pass: the pre-apply steps run and
// kubeone gets each set value under its aliases.
func TestRobotCredentialValidOrUnsetPasses(t *testing.T) {
	for _, ep := range robotEntryPoints {
		for _, set := range []bool{true, false} {
			t.Run(ep.name+map[bool]string{true: "/valid", false: "/unset"}[set], func(t *testing.T) {
				clearVarEnv(t)
				if set {
					t.Setenv("HROBOT_USER", robotValid["HROBOT_USER"])
					t.Setenv("HROBOT_PASSWORD", robotValid["HROBOT_PASSWORD"])
				}
				d, runner, errBuf, p := testDriver(t, nil)
				prepared := 0
				d.Hooks.PrepareApply = func(ctx context.Context, workDir, clusterYAML string) error {
					prepared++
					return nil
				}
				wd := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
				testutil.WriteFile(t, filepath.Join(wd, "kubeone.yaml"), "name: test-prod\n")
				if err := ep.call(t.Context(), d, wd); err != nil {
					t.Fatalf("%v\n%s", err, errBuf.String())
				}
				if errBuf.Len() != 0 {
					t.Errorf("stderr %q", errBuf.String())
				}
				if len(runner.calls) != 1 || runner.calls[0].Name != "kubeone" {
					t.Fatalf("calls %+v, want one kubeone run", runner.calls)
				}
				if ep.name == "Apply" && prepared != 1 {
					t.Errorf("the pre-apply steps ran %d times, want 1", prepared)
				}
				env := strings.Join(runner.calls[0].Env, "\n")
				want := ""
				if set {
					want = "ROBOT_USER=robot-user-not-real\nHETZNER_ROBOT_USER=robot-user-not-real\n" +
						"ROBOT_PASSWORD=robot-password-not-real\nHETZNER_ROBOT_PASSWORD=robot-password-not-real"
				}
				if env != want {
					t.Errorf("kubeone env\n%s\nwant\n%s", env, want)
				}
			})
		}
	}
}

// Destroy goes on after the refusal of Reset, as after any failed reset:
// the warning follows the two lines, kubeone does not run, and the
// provider destroy runs.
func TestDestroyAfterARobotCredentialRefusal(t *testing.T) {
	clearVarEnv(t)
	t.Setenv("HROBOT_USER", robotValid["HROBOT_USER"])
	t.Setenv("HROBOT_PASSWORD", robotBad["HROBOT_PASSWORD"][0])
	prov := &fakeProvider{}
	d, runner, errBuf, p := testDriver(t, prov)
	wd := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
	testutil.WriteFile(t, filepath.Join(wd, "kubeone.yaml"), "name: test-prod\n")
	if err := d.Destroy(t.Context(), "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if got, want := errBuf.String(), robotRefusal("HROBOT_PASSWORD")+"[warn] kubeone reset failed — continuing with infrastructure destroy\n"; got != want {
		t.Errorf("stderr\n%q\nwant\n%q", got, want)
	}
	if len(runner.calls) != 0 {
		t.Errorf("kubeone ran: %d calls", len(runner.calls))
	}
	if strings.Join(prov.log, ",") != "destroy:"+wd {
		t.Errorf("provider log %q, want the destroy", prov.log)
	}
}

// The common cause: a store entry with CRLF line ends. LoadProviderCreds
// keeps the CR, as bash $(cat …) does, and Apply refuses it before any step.
func TestApplyRefusesARobotCredentialFromACRLFStore(t *testing.T) {
	for _, name := range []string{"HROBOT_USER", "HROBOT_PASSWORD"} {
		t.Run(name, func(t *testing.T) {
			clearVarEnv(t)
			d, runner, errBuf, p := testDriver(t, nil)
			prepared := 0
			d.Hooks.PrepareApply = func(ctx context.Context, workDir, clusterYAML string) error {
				prepared++
				return nil
			}
			secrets := filepath.Join(p.Clusters, "test.lok8s.dev", "secrets")
			testutil.WriteFile(t, filepath.Join(secrets, "Secret.hetzner.provisioning."+name), robotValid[name]+"\r\n")
			other := robotOther[name]
			testutil.WriteFile(t, filepath.Join(secrets, "Secret.hetzner.provisioning."+other), robotValid[other]+"\n")
			if err := provision.LoadProviderCreds(p, "test.lok8s.dev"); err != nil {
				t.Fatal(err)
			}
			if got := os.Getenv(name); got != robotValid[name]+"\r" {
				t.Fatalf("%s = %q, want the value with its CR", name, got)
			}
			wd := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
			testutil.WriteFile(t, filepath.Join(wd, "kubeone.yaml"), "name: test-prod\n")
			err := d.Apply(t.Context(), wd, "")
			if !errors.Is(err, ui.ErrHandled) {
				t.Fatalf("error %v, want ErrHandled", err)
			}
			if got, want := errBuf.String(), robotRefusal(name); got != want {
				t.Errorf("stderr\n%q\nwant\n%q", got, want)
			}
			if len(runner.calls) != 0 || prepared != 0 {
				t.Errorf("a step ran: %d calls, %d pre-apply", len(runner.calls), prepared)
			}
		})
	}
}
