package cli

import (
	"strings"
	"testing"
)

// TestDoctorReportsPathSecretsAsItIs: the binary defaults PATH_SECRETS to
// nothing (every store is per domain), so doctor says so when the variable
// is unset and prints the directory when it is set (D35).
func TestDoctorReportsPathSecretsAsItIs(t *testing.T) {
	p := synthProject(t)
	t.Setenv("PATH", t.TempDir())
	p.SecretsEnv = ""
	stdout, _, _ := runLo(t, NewRoot(p), "doctor")
	if !strings.Contains(stdout, "ℹ PATH_SECRETS unset (per-domain stores; the flat store is retired)") {
		t.Errorf("unset: no info line:\n%s", stdout)
	}
	if strings.Contains(stdout, ".secrets") {
		t.Errorf("unset: doctor still names a flat store:\n%s", stdout)
	}

	p.SecretsEnv = p.Clusters
	stdout, _, _ = runLo(t, NewRoot(p), "doctor")
	if !strings.Contains(stdout, " PATH_SECRETS="+p.Clusters+"\n") {
		t.Errorf("set: no directory line:\n%s", stdout)
	}
}
