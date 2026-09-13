package scaffold

// cluster.go — the first cluster spec: the smallest
// clusters/<domain>/cluster.lok8s.yaml the readers accept (kind,
// metadata.name, spec.cluster.domain, spec.bootstrap), in the shape the
// lok8s-cluster-spec skill documents. `lo init project --cluster <domain>
// --driver <driver>` and the init wizard write it through the same
// function. Go-only: the frozen tree has no project scaffold.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// Driver is one entry of the driver menu.
type Driver struct {
	// Name is the --driver value.
	Name string
	// Kind is the spec's `kind`.
	Kind string
	// Label describes the driver in one line (the wizard's option text).
	Label string
	// Bootstrap is the explicit spec.bootstrap list the file carries: the
	// driver's default made visible (Lo ships no CNI, so its default is
	// cilium; the others default to none).
	Bootstrap []string
	// kubehz is the spec.kubehz block for the hosted control plane.
	kubehz bool
}

// Drivers is the menu, in the order the wizard offers it.
var Drivers = []Driver{
	{Name: "lo", Kind: "Lo", Label: "lo: kind on local Docker (dev clusters)", Bootstrap: []string{"cilium"}},
	{Name: "kubeone", Kind: "KubeOne", Label: "kubeone: KubeOne on VMs or bare metal (self-managed production)"},
	{Name: "capi", Kind: "Capi", Label: "capi: Cluster API managed clusters"},
	{Name: "kkp", Kind: "Kkp", Label: "kkp: Kubermatic (KKP) user clusters"},
	{Name: "kubehz-hosted", Kind: "KubeOne", Label: "kubehz-hosted: kubehz runs the control plane, workers on your Hetzner account", kubehz: true},
}

// DriverNames lists the --driver values.
func DriverNames() []string {
	names := make([]string, 0, len(Drivers))
	for _, d := range Drivers {
		names = append(names, d.Name)
	}
	return names
}

// DriverFor resolves a --driver value.
func DriverFor(name string) (Driver, bool) {
	for _, d := range Drivers {
		if d.Name == name {
			return d, true
		}
	}
	return Driver{}, false
}

// ValidateDomain is the domain rule `lo use` applies (domain.NameRe: the
// character allowlist that is also the path-traversal guard), with the
// empty value refused.
func ValidateDomain(name string) error {
	if name == "" {
		return fmt.Errorf("domain is required")
	}
	if !domain.NameRe.MatchString(name) {
		return fmt.Errorf("invalid domain name: %s (must match %s)", name, domain.NameRe.String())
	}
	return nil
}

// ClusterName is the metadata.name a domain implies: its first label
// (`demo.dev` → `demo`), the kubeconfig file name.
func ClusterName(dom string) string {
	name, _, _ := strings.Cut(dom, ".")
	return name
}

// ClusterSpec renders the minimal spec for a domain and driver.
func ClusterSpec(dom, driverName string) (string, error) {
	if err := ValidateDomain(dom); err != nil {
		return "", err
	}
	d, ok := DriverFor(driverName)
	if !ok {
		return "", fmt.Errorf("--driver must be one of %s, got %q", strings.Join(DriverNames(), "|"), driverName)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# clusters/%s/cluster.lok8s.yaml — the %s cluster (driver: %s).\n", dom, dom, d.Name)
	b.WriteString("# The minimal spec: every other key has a documented default (lo lint --notes\n")
	b.WriteString("# names the ones you can drop). Schema: docs/reference/specs.md.\n")
	b.WriteString("apiVersion: cluster.lok8s.dev/v1beta1\n")
	b.WriteString("kind: " + d.Kind + "\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + ClusterName(dom) + "\n")
	b.WriteString("spec:\n")
	b.WriteString("  cluster:\n")
	b.WriteString("    domain: " + dom + "\n")
	if d.kubehz {
		b.WriteString("  kubehz:\n")
		b.WriteString("    hosting: hosted                  # the platform runs the control plane\n")
		b.WriteString("    apiUrl: https://api.kubehz.cloud\n")
	}
	if len(d.Bootstrap) == 0 {
		b.WriteString("  bootstrap: []                      # addons, applied in order (lo addons lists them)\n")
	} else {
		b.WriteString("  bootstrap:                         # addons, applied in order (lo addons lists them)\n")
		for _, a := range d.Bootstrap {
			b.WriteString("    - " + a + "\n")
		}
	}
	return b.String(), nil
}

// WriteClusterSpec writes clusters/<domain>/cluster.lok8s.yaml under
// clusters (kept unless force).
func WriteClusterSpec(clusters, dom, driverName string, force bool, out, stderr io.Writer) error {
	content, err := ClusterSpec(dom, driverName)
	if err != nil {
		ui.ErrorTo(stderr, "%v", err)
		return ErrHandled
	}
	dir := filepath.Join(clusters, dom)
	file := filepath.Join(dir, "cluster.lok8s.yaml")
	if fsutil.FileExists(file) && !force {
		fmt.Fprintf(out, "Kept %s (exists; --force overwrites)\n", file)
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Scaffolded %s\n", file)
	return nil
}
