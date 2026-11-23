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
// the rendered kubeone.yaml with registry auth), and the lo driver's
// .provider/ (the cloud VM's provider state), .oidc/ (the apiserver auth
// config) and .registries.json (the registry slot plan). .docker.sock has
// no writer today; the repository's own .gitignore has it. The list is
// written here, not read from gitignoreEntries, so a removed entry fails
// the tests below. Two work dirs are not in the list: the lo driver writes
// its own .gitignore in .containerd/
// (TestGitignoreKeepsTheContainerdGitignore), and the kkp driver keeps
// only ids in .kkp/ (TestGitignoreTracksTheKkpIds).
var workDirEntries = []string{
	"**/clusters/*/.kubeone/",
	"**/clusters/*/.provider/",
	"**/clusters/*/.oidc/",
	"**/clusters/*/.registries.json",
	"**/clusters/*/.docker.sock",
}

// workDirFiles are files in the work dirs of clusters/x/, one or more for
// each entry of workDirEntries.
var workDirFiles = []string{
	"clusters/x/.kubeone/state",
	"clusters/x/.kubeone/x-kubeconfig",
	"clusters/x/.provider/y",
	"clusters/x/.oidc/auth-config.yaml",
	"clusters/x/.registries.json",
	"clusters/x/.docker.sock",
}

// handCases are lines of a .gitignore written by hand. Each has the entry it
// is written for, a file that the entry ignores, and whether git reads the
// line as the entry. git removes one carriage return at the end of the line,
// then the trailing spaces, and nothing else. TestGitignoreHandLinesIgnoredByGit
// checks each verdict with git.
var handCases = []struct {
	line, entry, path string
	present           bool
}{
	{"**/clusters/*/.provider/  ", "**/clusters/*/.provider/", "clusters/x/.provider/y", true},                   // trailing spaces
	{"**/clusters/*/.oidc/\r", "**/clusters/*/.oidc/", "clusters/x/.oidc/auth-config.yaml", true},                // a CRLF line end
	{"  **/clusters/*/.kubeone/", "**/clusters/*/.kubeone/", "clusters/x/.kubeone/state", false},                 // leading spaces
	{"**/clusters/*/.registries.json\t", "**/clusters/*/.registries.json", "clusters/x/.registries.json", false}, // a trailing tab
	{"**/clusters/*/.docker.sock\r ", "**/clusters/*/.docker.sock", "clusters/x/.docker.sock", false},            // a carriage return, then a space
	{".kustomize/\r\r", ".kustomize/", ".kustomize/plugin", false},                                               // two carriage returns
}

// handLines is the .gitignore of handCases, after a line that is no entry.
var handLines = func() string {
	s := "node_modules/\n"
	for _, c := range handCases {
		s += c.line + "\n"
	}
	return s
}()

// lineCounts counts each line of a .gitignore as it is written (no trim).
func lineCounts(raw []byte) map[string]int {
	counts := map[string]int{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		counts[line]++
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

// An entry is appended only when no line reads as it in git. A .gitignore
// with the lines of handCases keeps them first and unchanged. It gets each
// entry that no line stands for once (leading spaces, a trailing tab, a
// carriage return before a space, two carriage returns), and no entry that
// a line stands for (trailing spaces, a CRLF line end). A second run changes
// nothing and says so.
func TestGitignoreWorkDirEntriesAppendOnlyWhenAbsent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(path, []byte(handLines), 0o644); err != nil {
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
	if !strings.HasPrefix(string(first), handLines) {
		t.Errorf("the existing lines must stay first and unchanged:\n%q", first)
	}
	counts := lineCounts(first)
	wantCounts := map[string]int{}
	for _, e := range workDirEntries {
		wantCounts[e] = 1
	}
	for _, c := range handCases {
		wantCounts[c.entry] = 1
		if c.present {
			wantCounts[c.entry] = 0
		}
	}
	for want, n := range wantCounts {
		if counts[want] != n {
			t.Errorf("%q must be appended %d time(s) (found %d):\n%q", want, n, counts[want], first)
		}
	}
	for line, n := range counts {
		if line != "" && n > 1 {
			t.Errorf("%q is written %d times:\n%q", line, n, first)
		}
	}
	out.Reset()
	if err := EnsureGitignore(dir, &out); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(path); string(again) != string(first) {
		t.Errorf("a second run changed .gitignore:\n%q", again)
	}
	if want := "Kept " + path + " (entries present)\n"; !strings.Contains(out.String(), want) {
		t.Errorf("a second run must report %q, got:\n%s", want, out.String())
	}
}

// gitFunc runs git in a repository dir.
type gitFunc func(repo string, args ...string) (string, error)

// isolatedGit is git with no user or system config, so a global excludes
// file cannot make a check pass. Without git the test is skipped, and under
// CI it fails.
func isolatedGit(t *testing.T) gitFunc {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("CI=true: git not on PATH")
		}
		t.Skip("git not on PATH")
	}
	home := t.TempDir()
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+home, "XDG_CONFIG_HOME="+home, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	return func(repo string, args ...string) (string, error) {
		cmd := exec.Command(git, append([]string{"-C", repo}, args...)...)
		cmd.Env = env
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
}

// newRepo is an empty repository in a temp dir.
func newRepo(t *testing.T, git gitFunc) string {
	t.Helper()
	repo := t.TempDir()
	if out, err := git(repo, "init", "-q"); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return repo
}

// checkGitIgnores writes each path of ignored and kept under repo that does
// not exist yet, then requires that git ignores each path of ignored and
// tracks each path of kept (check-ignore exit 1).
func checkGitIgnores(t *testing.T, git gitFunc, repo string, ignored, kept []string) {
	t.Helper()
	for _, p := range append(append([]string(nil), ignored...), kept...) {
		full := filepath.Join(repo, filepath.FromSlash(p))
		if fsutil.FileExists(full) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range ignored {
		if out, err := git(repo, "check-ignore", "-q", "--", p); err != nil {
			t.Errorf("git does not ignore %s: %v\n%s", p, err, out)
		}
	}
	for _, p := range kept {
		out, err := git(repo, "check-ignore", "-q", "--", p)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Errorf("git must track %s (check-ignore exit 1), got %v\n%s", p, err, out)
		}
	}
}

// git ignores the files in the work dirs with the written .gitignore and
// still tracks the cluster spec. The entries start with **/, so a
// .gitignore at the repository root also ignores each work dir of a
// clusters/ tree in a sub dir (infra/clusters/).
func TestGitignoreWorkDirsIgnoredByGit(t *testing.T) {
	t.Parallel()
	git := isolatedGit(t)
	repo := newRepo(t, git)
	var out bytes.Buffer
	if err := EnsureGitignore(repo, &out); err != nil {
		t.Fatal(err)
	}
	ignored := append(append([]string(nil), workDirFiles...), "clusters/x/.kubeconfig/admin")
	for _, p := range workDirFiles {
		ignored = append(ignored, "infra/"+p)
	}
	checkGitIgnores(t, git, repo, ignored, []string{"clusters/x/cluster.lok8s.yaml", "lok8s.yaml"})
}

// A project in a sub dir of a larger repository: the scaffold runs for
// infra/ only and writes infra/.gitignore, and the .gitignore at the
// repository root has no lok8s line. git ignores the work dirs of
// infra/clusters/ with infra/.gitignore.
func TestGitignoreWorkDirsIgnoredInANestedProject(t *testing.T) {
	t.Parallel()
	git := isolatedGit(t)
	repo := newRepo(t, git)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	infra := filepath.Join(repo, "infra")
	if err := os.MkdirAll(infra, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := EnsureGitignore(infra, &out); err != nil {
		t.Fatal(err)
	}
	var ignored []string
	for _, p := range workDirFiles {
		ignored = append(ignored, "infra/"+p)
	}
	checkGitIgnores(t, git, repo, ignored, []string{"infra/clusters/x/cluster.lok8s.yaml", "infra/lok8s.yaml"})
}

// git ignores the lock of .sops.yaml and the temp files of its writes (the
// names of add-key and remove-key, and the name of init) at any depth, and
// still tracks .sops.yaml. The list is written here, not read from
// gitignoreEntries.
func TestGitignoreSopsLockAndTempFilesIgnoredByGit(t *testing.T) {
	t.Parallel()
	git := isolatedGit(t)
	repo := newRepo(t, git)
	var out bytes.Buffer
	if err := EnsureGitignore(repo, &out); err != nil {
		t.Fatal(err)
	}
	var ignored []string
	for _, p := range []string{".sops.yaml.lock", ".sops.yaml.tmp-AbC123", "..sops.yaml.tmp-1234567"} {
		ignored = append(ignored, p, "infra/"+p)
	}
	checkGitIgnores(t, git, repo, ignored, []string{".sops.yaml", "infra/.sops.yaml"})
}

// git reads each line of handCases as its verdict says: before the scaffold
// runs, git ignores the file of each line that stands for its entry, and
// not the others. After EnsureGitignore, git ignores every file.
func TestGitignoreHandLinesIgnoredByGit(t *testing.T) {
	t.Parallel()
	git := isolatedGit(t)
	repo := newRepo(t, git)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(handLines), 0o644); err != nil {
		t.Fatal(err)
	}
	var stands, others, all []string
	for _, c := range handCases {
		all = append(all, c.path)
		if c.present {
			stands = append(stands, c.path)
		} else {
			others = append(others, c.path)
		}
	}
	checkGitIgnores(t, git, repo, stands, others)
	var out bytes.Buffer
	if err := EnsureGitignore(repo, &out); err != nil {
		t.Fatal(err)
	}
	checkGitIgnores(t, git, repo, append(all, workDirFiles...), []string{"clusters/x/cluster.lok8s.yaml"})
}

// .kkp/ is not ignored: the kkp driver keeps only the cluster and project
// ids there (identifiers, not credentials; its kubeconfig goes to
// .kubeconfig/), and cluster_id is the handle that `lo down` needs, also in
// another checkout of the project.
func TestGitignoreTracksTheKkpIds(t *testing.T) {
	t.Parallel()
	git := isolatedGit(t)
	repo := newRepo(t, git)
	var out bytes.Buffer
	if err := EnsureGitignore(repo, &out); err != nil {
		t.Fatal(err)
	}
	checkGitIgnores(t, git, repo, nil, []string{"clusters/x/.kkp/cluster_id", "clusters/x/.kkp/project_id"})
}

// loContainerdGitignore is the .gitignore that the lo driver writes in
// clusters/<domain>/.containerd/ (writeCertsD in internal/driver/lo, bash
// lo::write_certs_d): it ignores the tree and keeps itself committable.
const loContainerdGitignore = "# Auto-generated by lok8s — containerd registry config, regenerated every\n" +
	"# `lo up`/provision from cluster node IPs + the local CA. The generated entries\n" +
	"# are never committed; this .gitignore is kept so the ignore travels with it.\n" +
	"*\n" +
	"!.gitignore\n"

// The scaffold has no .containerd/ entry, so the .gitignore that lo writes
// in .containerd/ stays committable: with it, git ignores the registry
// config and the CA copy, and tracks the .gitignore. A .containerd/ dir
// entry would ignore the .gitignore too.
func TestGitignoreKeepsTheContainerdGitignore(t *testing.T) {
	t.Parallel()
	git := isolatedGit(t)
	repo := newRepo(t, git)
	var out bytes.Buffer
	if err := EnsureGitignore(repo, &out); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(repo, "clusters", "x", ".containerd", ".gitignore")
	if err := os.MkdirAll(filepath.Dir(inner), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inner, []byte(loContainerdGitignore), 0o644); err != nil {
		t.Fatal(err)
	}
	checkGitIgnores(t, git, repo, []string{
		"clusters/x/.containerd/hosts.toml",
		"clusters/x/.containerd/.ca/rootCA.pem",
		"clusters/x/.containerd/certs.d/registry.local/hosts.toml",
		"clusters/x/.containerd/certs.d/.ca/rootCA.pem",
	}, []string{"clusters/x/.containerd/.gitignore"})
	if raw, _ := os.ReadFile(inner); string(raw) != loContainerdGitignore {
		t.Errorf("the check changed %s:\n%s", inner, raw)
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
