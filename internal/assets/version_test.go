package assets

// version_test.go covers the version fallback to the embedded VERSION.

import (
	"strings"
	"testing"
)

func TestVersionFallsBackToEmbedded(t *testing.T) {
	prev := BuildVersion
	t.Cleanup(func() { BuildVersion = prev })
	BuildVersion = ""
	emb, _ := readEmbedded("VERSION")
	if Version() != strings.TrimSpace(string(emb)) {
		t.Errorf("Version() = %q, embedded VERSION = %q", Version(), emb)
	}
	BuildVersion = "9.9.9"
	if Version() != "9.9.9" {
		t.Errorf("stamped version ignored: %s", Version())
	}
}
