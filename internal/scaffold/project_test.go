package scaffold

// project_test.go covers `lo init project`.

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
		wantErr   bool
	}{
		{"", true, false, false},
		{"mise", true, false, false},
		{"direnv", false, true, false},
		{"both", true, true, false},
		{"none", false, false, false},
		{"fish", false, false, true},
	}
	for _, c := range cases {
		t.Run("env="+c.env, func(t *testing.T) {
			dir := t.TempDir()
			var out, errOut bytes.Buffer
			err := Project(dir, "demo", dir, false, &out, &errOut, ProjectToolchain{Env: c.env, BVersion: "4.18.7"})
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error for --env %q", c.env)
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
				for _, want := range []string{`_.path = ["{{config_root}}/.bin"]`, `"github:fentas/b" = "4.18.7"`} {
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
				if !strings.Contains(string(raw), "PATH_add .bin") || strings.Contains(string(raw), "export PATH_BASE") {
					t.Errorf(".envrc content unexpected:\n%s", raw)
				}
			}
		})
	}

	// Existing env files are kept (never overwritten without --force).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mise.toml"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := Project(dir, "demo", dir, false, &out, &errOut, ProjectToolchain{}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "mise.toml")); string(raw) != "mine\n" {
		t.Fatalf("existing mise.toml was overwritten: %q", raw)
	}
}
