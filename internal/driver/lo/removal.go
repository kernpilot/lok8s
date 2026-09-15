package lo

// removal.go: the names the local driver's teardown removes, as data. The
// confirmation prompts (internal/cli/confirm.go) and `lo down` read them
// from here, and cleanupRegistries, RegistryClean and Destroy remove
// exactly these: one naming source, so a prompt can never drift from the
// deletion. Reading the plan writes nothing: it loads the registry file
// a run left behind and never generates one.

import (
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/fsutil"
)

// ErrNoRegistryFile is the Removal error when the domain has no
// .registries.json yet (a run generates it; a plan does not).
var ErrNoRegistryFile = errors.New("no .registries.json for the domain yet")

// Removal names what a teardown of the domain's registry set removes.
type Removal struct {
	// Registries are the project registries: one container and one data
	// volume per name (cleanupRegistries). A shared set's mirrors are not
	// here, they live on the shared network.
	Registries []string
	// TLSVolume is the set's certificate volume, "" when TLS is off.
	TLSVolume string
	// Mirrors are the shared-network names of every mirror
	// (`lok8s-registry-<name>`), and Network the file's network: what
	// `lo registry clean --shared` removes, shared set or not.
	Mirrors []string
	Network string
	// IsShared reports a set whose mirrors live on the shared network.
	IsShared bool
}

// RegistryRemoval lists what a teardown of the domain's registry set
// removes, from the registry file a run left behind: LOK8S_REGISTRY_JSON
// when it names an existing file (a Tilt subshell, the way registryInit
// reads it), else the domain's own file. It runs no docker command and
// writes nothing; ErrNoRegistryFile when there is no file to read.
func RegistryRemoval(p *config.Paths, domain string) (*Removal, error) {
	if path := getenv("LOK8S_REGISTRY_JSON"); path != "" && fsutil.FileExists(path) {
		return RegistryRemovalAt(path)
	}
	return RegistryRemovalAt(RegistryFilePath(p, domain))
}

// RegistryFilePath is the domain's registry file, clusters/<domain>/
// .registries.json.
func RegistryFilePath(p *config.Paths, domain string) string {
	return filepath.Join(p.Clusters, domain, ".registries.json")
}

// RegistryRemovalAt is RegistryRemoval over one registry file.
func RegistryRemovalAt(path string) (*Removal, error) {
	rf, err := loadRegistryFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNoRegistryFile
		}
		return nil, err
	}
	r := &Removal{Network: rf.Network.Name, IsShared: rf.Shared}
	for _, reg := range rf.Registries {
		if reg.Name == "" {
			continue // a registry without a name names nothing (the bash skipped it)
		}
		if reg.Type == "mirror" {
			r.Mirrors = append(r.Mirrors, SharedRegistryPrefix+reg.Name)
			if rf.Shared {
				continue
			}
		}
		if rf.ProjectNetwork == "" {
			continue // no project network in the file: nothing is named
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
