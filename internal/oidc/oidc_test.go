package oidc

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvIssuer, EnvClientID, EnvUsernameClaim,
		EnvUsernamePrefix, EnvGroupsClaim, EnvGroupsPrefix, EnvCABundle} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestLoadSpecDefaults(t *testing.T) {
	clearEnv(t)
	spec := filepath.Join(t.TempDir(), "cluster.lok8s.yaml")
	if err := os.WriteFile(spec, []byte(
		"spec:\n  oidc:\n    issuer: https://id.example\n    clientID: kubectl\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := LoadSpec(spec, &buf); err != nil {
		t.Fatalf("LoadSpec: %v (%s)", err, buf.String())
	}
	if !Enabled() {
		t.Fatal("Enabled() = false after a full spec")
	}
	for env, want := range map[string]string{
		EnvUsernameClaim:  "sub",
		EnvUsernamePrefix: "oidc:",
		EnvGroupsClaim:    "groups",
		EnvGroupsPrefix:   "oidc:",
	} {
		if got := os.Getenv(env); got != want {
			t.Errorf("%s = %q, want %q", env, got, want)
		}
	}
}

func TestLoadSpecExplicitEmptyPrefixSurvives(t *testing.T) {
	clearEnv(t)
	// yq `// "oidc:"` fires on missing/null, NOT on an explicit "" — an
	// explicit empty prefix is a deliberate "no prefix".
	spec := filepath.Join(t.TempDir(), "cluster.lok8s.yaml")
	if err := os.WriteFile(spec, []byte(
		"spec:\n  oidc:\n    issuer: https://id.example\n    clientID: kubectl\n    usernamePrefix: \"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := LoadSpec(spec, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(EnvUsernamePrefix); got != "" {
		t.Errorf("usernamePrefix = %q, want explicit empty", got)
	}
}

func TestLoadSpecHTTPIssuerRejected(t *testing.T) {
	clearEnv(t)
	spec := filepath.Join(t.TempDir(), "cluster.lok8s.yaml")
	if err := os.WriteFile(spec, []byte(
		"spec:\n  oidc:\n    issuer: http://plain.example\n    clientID: kubectl\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := LoadSpec(spec, &buf); err == nil {
		t.Fatal("LoadSpec accepted an http:// issuer")
	}
	want := "spec.oidc.issuer must be an https:// URL, got 'http://plain.example'"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("stderr missing %q; got %s", want, buf.String())
	}
}

func TestLoadSpecMissingFile(t *testing.T) {
	clearEnv(t)
	var buf bytes.Buffer
	if err := LoadSpec(filepath.Join(t.TempDir(), "nope.yaml"), &buf); err == nil {
		t.Fatal("LoadSpec accepted a missing file")
	}
	if !strings.Contains(buf.String(), "oidc: cluster spec not found:") {
		t.Errorf("stderr = %s", buf.String())
	}
}
