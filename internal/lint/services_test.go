package lint

// services_test.go covers the services.yaml checks.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

func TestServicesImageRegistryExclusive(t *testing.T) {
	t.Parallel()
	l, base, _, errOut := newLinter(t)
	testutil.WriteFile(t, filepath.Join(base, "services.yaml"),
		"services:\n  app:\n    image: pinned:1\n    registry:\n      endpoint: r\n")

	if l.services() {
		t.Fatal("services() = ok, want error")
	}
	want := "  services.yaml: services.app: 'image' and 'registry' are mutually exclusive"
	if !strings.Contains(errOut.String(), want) {
		t.Errorf("stderr missing %q; got:\n%s", want, errOut.String())
	}
}
