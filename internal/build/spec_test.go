package build

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactsMode(t *testing.T) {
	cases := []struct {
		name string
		spec string // "" = no spec file
		want string
	}{
		{"no spec file", "", "single"},
		{"empty spec", "kind: Lo\n", "single"},
		{"artifacts split", "spec:\n  build:\n    artifacts: split\n", "split"},
		{"artifacts single", "spec:\n  build:\n    artifacts: single\n", "single"},
		{"gitops provider implies split", "spec:\n  gitops:\n    provider: flux\n", "split"},
		{"provider + explicit single pins single", "spec:\n  gitops:\n    provider: flux\n  build:\n    artifacts: single\n", "single"},
		{"provider + explicit split", "spec:\n  gitops:\n    provider: flux\n  build:\n    artifacts: split\n", "split"},
		{"unknown artifacts value defaults single", "spec:\n  build:\n    artifacts: bogus\n", "single"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.spec != "" {
				writeFileT(t, filepath.Join(dir, "cluster.lok8s.yaml"), tc.spec)
			}
			if got := ArtifactsMode(dir); got != tc.want {
				t.Errorf("ArtifactsMode = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestArtifactsModeDeploySpec(t *testing.T) {
	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, "deploy.lok8s.yaml"), "spec:\n  gitops:\n    provider: flux\n")
	if got := ArtifactsMode(dir); got != "split" {
		t.Errorf("deploy.lok8s.yaml must be read as the spec file, got %q", got)
	}
}

func TestEncryptMode(t *testing.T) {
	cases := []struct {
		name     string
		spec     string
		wantType string
		wantOn   string
		wantErr  string
	}{
		{"no spec -> defaults", "", "sops", "change", ""},
		{"absent block -> defaults", "kind: Lo\n", "sops", "change", ""},
		{"explicit empty strings -> defaults", "spec:\n  build:\n    encrypt:\n      type: \"\"\n      on: \"\"\n", "sops", "change", ""},
		{"on always", "spec:\n  build:\n    encrypt:\n      on: always\n", "sops", "always", ""},
		{"bad type", "spec:\n  build:\n    encrypt:\n      type: gpg\n", "", "", "spec.build.encrypt.type 'gpg' is not supported (only 'sops')"},
		{"bad on", "spec:\n  build:\n    encrypt:\n      on: never\n", "", "", "spec.build.encrypt.on 'never' is invalid (use 'change' or 'always')"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.spec != "" {
				writeFileT(t, filepath.Join(dir, "cluster.lok8s.yaml"), tc.spec)
			}
			encType, encOn, err := EncryptMode(dir)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("EncryptMode err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if encType != tc.wantType || encOn != tc.wantOn {
				t.Errorf("EncryptMode = (%q,%q), want (%q,%q)", encType, encOn, tc.wantType, tc.wantOn)
			}
		})
	}
}

func TestNoSecretsEffective(t *testing.T) {
	cases := []struct {
		name string
		flag bool
		env  string // "" = unset
		want bool
	}{
		{"flag on wins", true, "", true},
		{"flag on with env 0 still wins", true, "0", true},
		{"env 1 honored", false, "1", true},
		{"env 0 off", false, "0", false},
		{"unset off", false, "", false},
		{"env junk off", false, "yes", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				t.Setenv("LOK8S_BUILD_NO_SECRETS", "") // register restore
				os.Unsetenv("LOK8S_BUILD_NO_SECRETS")
			} else {
				t.Setenv("LOK8S_BUILD_NO_SECRETS", tc.env)
			}
			if got := NoSecretsEffective(tc.flag); got != tc.want {
				t.Errorf("NoSecretsEffective(%v) with env %q = %v, want %v", tc.flag, tc.env, got, tc.want)
			}
		})
	}
}

func TestGitopsAgeRecipients(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "cluster.lok8s.yaml")
	writeFileT(t, spec, "spec:\n  gitops:\n    age:\n      - age1aaa\n      - age1bbb\n")
	if got := gitopsAgeRecipients(spec); got != "age1aaa,age1bbb" {
		t.Errorf("gitopsAgeRecipients = %q", got)
	}
	writeFileT(t, spec, "spec: {}\n")
	if got := gitopsAgeRecipients(spec); got != "" {
		t.Errorf("absent age list must join to empty, got %q", got)
	}
}
