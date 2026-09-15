package lo

// removal.go: the names the local driver's teardown removes, as data. The
// confirmation prompts (internal/cli/confirm.go) read them from here, and
// cleanupRegistries, RegistryClean and Destroy remove exactly these, so a
// prompt can never drift from the deletion: one naming source.

import (
	"io"
)

// Removal names what a teardown of the domain's registry set removes.
type Removal struct {
	// Registries are the project registries: one container and one data
	// volume per name (cleanupRegistries). A shared mirror is not here.
	Registries []string
	// TLSVolume is the set's certificate volume, "" when TLS is off.
	TLSVolume string
	// Shared lists the shared mirrors (container and volume per name) and
	// Network the shared network: what `lo registry clean --shared` adds.
	Shared  []string
	Network string
	// IsShared reports a set whose mirrors live on the shared network.
	IsShared bool
}

// Removal resolves the domain's registry set (the same init the registry
// commands run, so a non-Lo domain fails here the way the command does)
// and lists what a teardown removes. It runs no docker command.
func (d *Driver) Removal(domain string, errOut io.Writer) (*Removal, error) {
	if err := d.registryInit(domain, errOut); err != nil {
		return nil, err
	}
	rf, err := regFile()
	if err != nil {
		return nil, err
	}
	r := &Removal{Network: rf.Network.Name, IsShared: rf.Shared}
	for _, reg := range rf.Registries {
		if rf.Shared && reg.Type == "mirror" {
			r.Shared = append(r.Shared, SharedRegistryPrefix+reg.Name)
			continue
		}
		name, _ := rf.containerFor(reg.Name)
		r.Registries = append(r.Registries, name)
	}
	if rf.TLS {
		r.TLSVolume = rf.tlsVolume()
	}
	return r, nil
}

// ProxyContainer is the name of the cluster's proxy container, which
// Destroy removes with the cluster.
func ProxyContainer(clusterName string) string { return clusterName + "-proxy" }
