package provision

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/config"
)

// LoadProviderCreds auto-loads managed provider credentials from the
// per-domain secret store (clusters/<domain>/secrets/
// Secret.hetzner.provisioning.<KEY>) so provisioning works without the
// operator hand-exporting them: the hetzner provider needs HCLOUD_TOKEN,
// and the bare-metal worker + CCM robot mode need the Robot user/pass.
// Fills only what's missing; no-op when the secret isn't present
// (non-hetzner providers, or a fresh checkout before `lo secrets decrypt`).
// Idempotent + phase-agnostic: shared by Dispatch AND `lo recover`.
//
// ALWAYS returns nil — the bash original's trailing `[[ -n ]] && export`
// returned 1 when the Robot creds were absent (cloud-only provisions) and
// aborted the unguarded callers under `set -e`; the "returns 0 when robot
// creds absent" unit test pins this contract.
func LoadProviderCreds(p *config.Paths, domainName string) error {
	secretsDir := filepath.Join(p.Clusters, domainName, "secrets")
	for _, k := range []string{"HCLOUD_TOKEN", "HROBOT_USER", "HROBOT_PASSWORD"} {
		if os.Getenv(k) != "" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(secretsDir, "Secret.hetzner.provisioning."+k))
		if err != nil {
			continue
		}
		// Trailing newlines only — matching bash's $(cat …) substitution.
		v := strings.TrimRight(string(raw), "\n")
		if v != "" {
			os.Setenv(k, v)
		}
	}
	if u := os.Getenv("HROBOT_USER"); u != "" {
		os.Setenv("ROBOT_USER", u)
		os.Setenv("HETZNER_ROBOT_USER", u)
	}
	if pw := os.Getenv("HROBOT_PASSWORD"); pw != "" {
		os.Setenv("ROBOT_PASSWORD", pw)
		os.Setenv("HETZNER_ROBOT_PASSWORD", pw)
	}
	return nil
}
