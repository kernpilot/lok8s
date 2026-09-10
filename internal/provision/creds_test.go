package provision

// creds_test.go covers the provider credential loading.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

func clearCredsEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"HCLOUD_TOKEN", "HROBOT_USER", "HROBOT_PASSWORD",
		"ROBOT_USER", "ROBOT_PASSWORD", "HETZNER_ROBOT_USER", "HETZNER_ROBOT_PASSWORD",
	} {
		t.Setenv(k, "") // registers restore of the original value
		os.Unsetenv(k)
	}
}

// bats: "provision::load_provider_creds returns 0 when robot creds absent
// (set -e regression)" — pre-fix, the trailing `[[ -n ]] && export` made
// the bash function return 1 with no creds.
func TestLoadProviderCredsNilWhenAbsent(t *testing.T) {
	clearCredsEnv(t)
	p := testPaths(t)
	if err := LoadProviderCreds(p, "test.lok8s.dev"); err != nil {
		t.Fatalf("must be nil-error when robot creds absent, got %v", err)
	}
}

// bats: "provision::load_provider_creds loads creds from the per-domain store"
func TestLoadProviderCredsLoadsStore(t *testing.T) {
	clearCredsEnv(t)
	p := testPaths(t)
	secd := filepath.Join(p.Clusters, "test.lok8s.dev", "secrets")
	testutil.WriteFile(t, filepath.Join(secd, "Secret.hetzner.provisioning.HCLOUD_TOKEN"), "tok-123")
	testutil.WriteFile(t, filepath.Join(secd, "Secret.hetzner.provisioning.HROBOT_USER"), "rob-usr")
	testutil.WriteFile(t, filepath.Join(secd, "Secret.hetzner.provisioning.HROBOT_PASSWORD"), "rob-pwd")

	if err := LoadProviderCreds(p, "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"HCLOUD_TOKEN":           "tok-123",
		"ROBOT_USER":             "rob-usr",
		"HETZNER_ROBOT_USER":     "rob-usr",
		"ROBOT_PASSWORD":         "rob-pwd",
		"HETZNER_ROBOT_PASSWORD": "rob-pwd",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// bats: "provision::load_provider_creds does not clobber preset env"
func TestLoadProviderCredsKeepsPresetEnv(t *testing.T) {
	clearCredsEnv(t)
	p := testPaths(t)
	secd := filepath.Join(p.Clusters, "test.lok8s.dev", "secrets")
	testutil.WriteFile(t, filepath.Join(secd, "Secret.hetzner.provisioning.HCLOUD_TOKEN"), "store-token")
	t.Setenv("HCLOUD_TOKEN", "env-token")

	if err := LoadProviderCreds(p, "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("HCLOUD_TOKEN"); got != "env-token" {
		t.Fatalf("HCLOUD_TOKEN = %q, want env-token", got)
	}
}
