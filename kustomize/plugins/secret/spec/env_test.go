package spec

import (
	"strings"
	"testing"
)

// optional / default / passwd are mutually exclusive — at most one.
func TestEnvEntry_FallbackMutualExclusion(t *testing.T) {
	src := `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: x, namespace: y}
env:
  K: { var: V, optional: true, default: "d" }
`
	_, err := Decode(strings.NewReader(src))
	if err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Errorf("expected mutual-exclusion error, got %v", err)
	}
}
