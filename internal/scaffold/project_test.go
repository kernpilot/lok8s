package scaffold

// project_test.go covers `lo init project`: files only, one environment
// file from one spec, idempotent, --force.

import (
	"bytes"
	"os"
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
