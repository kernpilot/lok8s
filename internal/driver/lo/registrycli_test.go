package lo

// registrycli_test.go covers the `lo registry` verbs in registrycli.go.

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRegistryCleanSharedDetachesHolders(t *testing.T) {
	d, _, fd, _, _, _ := lifecycleDriver(t)
	// A foreign holder is attached — exactly the state the
	// squatted-registry error sends the operator here to fix. Keeping the
	// network would send the next lo up straight back into the same error.
	fd.setNetworkMeta("lok8s-registries", "10.125.200.0/24", "")
	fd.addMember("lok8s-registries", "10.125.200.2/24", "foreign-node")

	var errBuf bytes.Buffer
	if err := d.RegistryClean(t.Context(), "test.lok8s.dev", true, &errBuf); err != nil {
		t.Fatalf("RegistryClean: %v\n%s", err, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "detaching 'foreign-node'") {
		t.Fatalf("detach not announced:\n%s", errBuf.String())
	}
	if _, err := os.Stat(fd.networkPath("lok8s-registries")); err == nil {
		t.Fatal("the network survived clean --shared — the recommended remediation loops back into the same squatted-IP failure forever")
	}
}
