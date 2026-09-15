package lo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegistryRemovalAtListsWhatTheTeardownRemoves: the project set by
// containerFor (an empty name and a missing project network name
// nothing), every mirror by its shared name (an empty name included, the
// way RegistryClean --shared removes it), the certificate volume.
func TestRegistryRemovalAtListsWhatTheTeardownRemoves(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, ".registries.json")
	os.WriteFile(file, []byte(`{"shared":false,"tls":true,"project_network":"alpha","network":{"name":"alpha"},"registries":[{"name":"build","type":"build"},{"name":"io-docker","type":"mirror"},{"name":"","type":"mirror"},{"name":"","type":"build"}]}`), 0o644)
	rem, err := RegistryRemovalAt(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rem.Registries, ","); got != "alpha-registry-build,alpha-registry-io-docker" {
		t.Errorf("Registries = %s", got)
	}
	if got := strings.Join(rem.Mirrors, ","); got != "lok8s-registry-io-docker,lok8s-registry-" {
		t.Errorf("Mirrors = %s", got)
	}
	if rem.TLSVolume != "alpha-registry-tls" || rem.Network != "alpha" || rem.IsShared {
		t.Errorf("rem = %+v", rem)
	}
	if _, err := RegistryRemovalAt(filepath.Join(dir, "missing.json")); !errors.Is(err, ErrNoRegistryFile) {
		t.Errorf("missing file: err %v, want ErrNoRegistryFile", err)
	}
}
