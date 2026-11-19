package scaffold

// project_test.go covers `lo init project`: files only, one environment
// file from one spec, idempotent, --force.

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/fsutil"
)

func TestProjectEnvFiles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		env       string
		wantMise  bool
		wantEnvrc bool
		wantErr   string
	}{
		{"", true, false, ""},
		{"mise", true, false, ""},
		{"direnv", false, true, ""},
		{"none", false, false, ""},
		{"both", false, false, "--env both was removed: one file per project. Use --env mise (the default) or --env direnv"},
		{"fish", false, false, `--env must be one of mise|direnv|none, got "fish"`},
	}
	for _, c := range cases {
		t.Run("env="+c.env, func(t *testing.T) {
			dir := t.TempDir()
			var out, errOut bytes.Buffer
			err := Project(dir, ProjectOptions{Dir: dir, Name: "demo", Env: c.env, BVersion: "4.18.7"}, &out, &errOut)
			if c.wantErr != "" {
				if err == nil || err.Error() != c.wantErr {
					t.Fatalf("--env %q: err = %v, want %q", c.env, err, c.wantErr)
				}
				// A refused --env writes nothing at all.
				if entries, _ := os.ReadDir(dir); len(entries) != 0 {
					t.Fatalf("--env %q wrote files: %v", c.env, entries)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			mise, envrc := fsutil.FileExists(filepath.Join(dir, "mise.toml")), fsutil.FileExists(filepath.Join(dir, ".envrc"))
			if mise != c.wantMise || envrc != c.wantEnvrc {
				t.Fatalf("env=%q: mise.toml=%v .envrc=%v, want %v/%v", c.env, mise, envrc, c.wantMise, c.wantEnvrc)
			}
			if c.wantMise {
				raw, _ := os.ReadFile(filepath.Join(dir, "mise.toml"))
				s := string(raw)
				for _, want := range []string{`_.path = ["{{config_root}}/.bin"]`, `"github:fentas/b" = "4.18.7"`, "mise.toml — demo"} {
					if !strings.Contains(s, want) {
						t.Errorf("mise.toml missing %q", want)
					}
				}
				if strings.Contains(s, "PATH_BASE") {
					t.Error("mise.toml must not pin PATH_* variables")
				}
			}
			if c.wantEnvrc {
				raw, _ := os.ReadFile(filepath.Join(dir, ".envrc"))
				if !strings.Contains(string(raw), "PATH_add .bin\n") || strings.Contains(string(raw), "export PATH_BASE") {
					t.Errorf(".envrc content unexpected:\n%s", raw)
				}
			}
			for _, absent := range []string{".bin/b.yaml", ".bin", ".lok8s"} {
				if _, err := os.Stat(filepath.Join(dir, absent)); err == nil {
					t.Errorf("init project wrote %s (files only: the toolchain is lo toolchain install)", absent)
				}
			}
			if !strings.Contains(out.String(), "lo toolchain install") {
				t.Errorf("next steps do not name lo toolchain install:\n%s", out.String())
			}
		})
	}
}

// Both writers render the one EnvSpec: the same project name, the same
// PATH entry, no PATH_* pin in either.
func TestEnvWritersRenderOneSpec(t *testing.T) {
	t.Parallel()
	spec := EnvSpec{Project: "p", BinRel: "tools/bin", BVersion: "9.9.9"}
	m, e := miseTOML(spec), envrc(spec)
	if !strings.Contains(m, `_.path = ["{{config_root}}/tools/bin"]`) || !strings.Contains(e, "PATH_add tools/bin\n") {
		t.Errorf("BinRel not rendered by both writers:\n%s\n%s", m, e)
	}
	if !strings.Contains(m, "— p:") || !strings.Contains(e, "— p:") {
		t.Errorf("project name not rendered by both writers:\n%s\n%s", m, e)
	}
	if !strings.Contains(m, `"9.9.9"`) {
		t.Errorf("BVersion not rendered by mise:\n%s", m)
	}
	for _, body := range []string{m, e} {
		if strings.Contains(body, "PATH_BASE=") || strings.Contains(body, "export PATH_") {
			t.Errorf("a PATH_* pin in an env file:\n%s", body)
		}
	}
	if len(envWriters) != len(EnvFiles)-1 {
		t.Errorf("envWriters (%d) and EnvFiles (%v) disagree", len(envWriters), EnvFiles)
	}
}

// The split's scratch dirs (clusters/<domain>/.artifacts-tmp.* and
// .artifacts-stage.*) are ignored in every consumer project: `lo init
// project` and EnsureGitignore write both prefixes, once.
func TestGitignoreEntriesCoverTheSplitScratch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var out bytes.Buffer
	if err := EnsureGitignore(dir, &out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**/clusters/*/.artifacts-tmp.*/\n", "**/clusters/*/.artifacts-stage.*/\n"} {
		if strings.Count(string(raw), want) != 1 {
			t.Errorf(".gitignore must carry %q exactly once:\n%s", want, raw)
		}
	}
	if err := EnsureGitignore(dir, &out); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); string(again) != string(raw) {
		t.Errorf("a re-run changed .gitignore:\n%s", again)
	}
}

// workDirEntries are the drivers' per-cluster work dirs under
// clusters/<domain>/: .kubeone/ (the kubeone driver: the admin kubeconfig,
// the rendered kubeone.yaml with registry auth), .kkp/ (the kkp driver's
// saved cluster and project ids), and the lo driver's .provider/ (the cloud
// VM's provider state), .oidc/ (the apiserver auth config), .containerd/
// (the registry certs.d tree), .registries.json (the registry slot plan)
// and .docker.sock. The list is written here, not read from
// gitignoreEntries, so a removed entry fails the tests below.
var workDirEntries = []string{
	"**/clusters/*/.kubeone/",
	"**/clusters/*/.kkp/",
	"**/clusters/*/.provider/",
	"**/clusters/*/.oidc/",
	"**/clusters/*/.containerd/",
	"**/clusters/*/.registries.json",
	"**/clusters/*/.docker.sock",
}

// lineCounts counts each line of a .gitignore, trimmed (appendGitignore
// compares trimmed lines too).
func lineCounts(raw []byte) map[string]int {
	counts := map[string]int{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		counts[strings.TrimSpace(line)]++
	}
	return counts
}

// `lo init project` writes an entry for each driver work dir, once, so a
// `git add -A` in the project does not commit cluster state or credentials.
func TestGitignoreEntriesCoverTheDriverWorkDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	if err := Project(dir, ProjectOptions{Name: "demo"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	counts := lineCounts(raw)
	for _, want := range workDirEntries {
		if counts[want] != 1 {
			t.Errorf(".gitignore must carry %q exactly once (found %d):\n%s", want, counts[want], raw)
		}
	}
}

// An entry is appended only when it is absent: a .gitignore that already
// has some work-dir entries (written by hand, with spaces around one) keeps
// its lines first and unchanged and gets only the missing entries; a
// second run changes nothing and says so. No line is written twice.
func TestGitignoreWorkDirEntriesAppendOnlyWhenAbsent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")
	mine := "node_modules/\n**/clusters/*/.provider/\n  **/clusters/*/.kubeone/  \n"
	if err := os.WriteFile(path, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := EnsureGitignore(dir, &out); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(first), mine) {
		t.Errorf("the existing lines must stay first and unchanged:\n%s", first)
	}
	counts := lineCounts(first)
	for _, want := range workDirEntries {
		if counts[want] != 1 {
			t.Errorf(".gitignore must carry %q exactly once (found %d):\n%s", want, counts[want], first)
		}
	}
	for line, n := range counts {
		if line != "" && n > 1 {
			t.Errorf("%q is written %d times:\n%s", line, n, first)
		}
	}
	out.Reset()
	if err := EnsureGitignore(dir, &out); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(path); string(again) != string(first) {
		t.Errorf("a second run changed .gitignore:\n%s", again)
	}
	if want := "Kept " + path + " (entries present)\n"; !strings.Contains(out.String(), want) {
		t.Errorf("a second run must report %q, got:\n%s", want, out.String())
	}
}

// git itself ignores the files in the work dirs with the written
// .gitignore, in a project at the repository root and in one nested in a
// larger repository, and still tracks the cluster spec. git runs with no
// user or system config, so a global excludes file cannot make this pass.
func TestGitignoreWorkDirsIgnoredByGit(t *testing.T) {
	t.Parallel()
	git, err := exec.LookPath("git")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("CI=true: git not on PATH")
		}
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	home := t.TempDir()
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+home, "XDG_CONFIG_HOME="+home, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	run := func(args ...string) (string, error) {
		cmd := exec.Command(git, append([]string{"-C", repo}, args...)...)
		cmd.Env = env
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	if out, err := run("init", "-q"); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	var out bytes.Buffer
	if err := EnsureGitignore(repo, &out); err != nil {
		t.Fatal(err)
	}
	ignored := []string{
		"clusters/x/.kubeone/state",
		"clusters/x/.kubeone/x-kubeconfig",
		"clusters/x/.kkp/cluster_id",
		"clusters/x/.provider/y",
		"clusters/x/.oidc/auth-config.yaml",
		"clusters/x/.containerd/certs.d/hosts.toml",
		"clusters/x/.registries.json",
		"clusters/x/.docker.sock",
		"clusters/x/.kubeconfig/admin",
		"infra/clusters/x/.kubeone/state",
		"infra/clusters/x/.provider/y",
	}
	kept := []string{"clusters/x/cluster.lok8s.yaml", "lok8s.yaml"}
	for _, p := range append(append([]string(nil), ignored...), kept...) {
		full := filepath.Join(repo, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range ignored {
		if out, err := run("check-ignore", "-q", "--", p); err != nil {
			t.Errorf("git does not ignore %s: %v\n%s", p, err, out)
		}
	}
	for _, p := range kept {
		out, err := run("check-ignore", "-q", "--", p)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Errorf("git must track %s (check-ignore exit 1), got %v\n%s", p, err, out)
		}
	}
}

// A re-run keeps every file; --force rewrites them; .gitignore is only
// ever appended to.
func TestProjectIdempotentAndForce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	if err := Project(dir, ProjectOptions{Name: "demo"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	first := map[string]string{}
	for _, f := range []string{"lok8s.yaml", "mise.toml", ".gitignore", "clusters/.gitkeep"} {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("%s not written: %v", f, err)
		}
		first[f] = string(raw)
	}
	if err := os.WriteFile(filepath.Join(dir, "mise.toml"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Project(dir, ProjectOptions{Name: "demo"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "mise.toml")); string(raw) != "mine\n" {
		t.Fatalf("existing mise.toml was overwritten: %q", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); string(raw) != first[".gitignore"] {
		t.Fatalf(".gitignore changed on a re-run:\n%s", raw)
	}
	if !strings.Contains(out.String(), "Kept "+filepath.Join(dir, "lok8s.yaml")+" (exists; --force overwrites)\n") {
		t.Errorf("re-run did not report the kept file:\n%s", out.String())
	}
	if err := Project(dir, ProjectOptions{Name: "demo", Force: true}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "mise.toml")); string(raw) != first["mise.toml"] {
		t.Fatalf("--force did not rewrite mise.toml: %q", raw)
	}
}

// One environment file per project: with the other writer's file present
// the selected one is kept out and the line names the existing file;
// --force writes it beside. Both directions.
func TestProjectEnvFileOtherWriterPresent(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ existing, env, wantFile string }{
		{"mise.toml", "direnv", ".envrc"},
		{".envrc", "mise", "mise.toml"},
	} {
		t.Run(c.existing+"->"+c.env, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.existing), []byte("mine\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			if err := Project(dir, ProjectOptions{Name: "demo", Env: c.env}, &out, &errOut); err != nil {
				t.Fatal(err)
			}
			if fsutil.FileExists(filepath.Join(dir, c.wantFile)) {
				t.Errorf("%s written beside %s without --force", c.wantFile, c.existing)
			}
			want := "Kept " + filepath.Join(dir, c.existing) + " (exists; one environment file per project, so " + c.wantFile + " is not written; --force writes it beside)\n"
			if !strings.Contains(out.String(), want) {
				t.Errorf("no kept line naming %s:\n%s", c.existing, out.String())
			}
			out.Reset()
			if err := Project(dir, ProjectOptions{Name: "demo", Env: c.env, Force: true}, &out, &errOut); err != nil {
				t.Fatal(err)
			}
			if !fsutil.FileExists(filepath.Join(dir, c.wantFile)) {
				t.Errorf("--force did not write %s", c.wantFile)
			}
			if !strings.Contains(out.String(), "Writing "+c.wantFile+" beside "+c.existing+" (--force;") {
				t.Errorf("no beside line under --force:\n%s", out.String())
			}
			if raw, _ := os.ReadFile(filepath.Join(dir, c.existing)); string(raw) != "mine\n" {
				t.Errorf("--force touched the other writer's file: %q", raw)
			}
		})
	}
}
