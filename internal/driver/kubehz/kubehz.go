// Package kubehz is the Go port of the kubehz driver (.lok8s/drivers/kubehz/
// main): Spaces on the kubehz shared control plane (spec `kind: Kubehz`,
// spec.kubehz.hosting: shared).
//
// The thinnest driver in the tree, on purpose: a Space has no
// infrastructure of its own. The platform operates the control plane; you
// bring machines and join them as nodes. Provision = create/adopt the Space
// + mint join tickets; destroy = deregister it. There is no kubeconfig to
// extract — access is via your kubehz login (OIDC).
//
// Requires: spec.kubehz.hosting: shared, spec.kubehz.apiUrl, KUBEHZ_TOKEN.
package kubehz

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/driver"
	platform "github.com/kernpilot/lok8s/internal/kubehz"
	"github.com/kernpilot/lok8s/internal/ui"
)

// Name is the driver's registry name (spec `kind: Kubehz`).
const Name = "kubehz"

func init() {
	driver.Register(Name, func(deps *driver.Deps) (driver.Driver, error) {
		return New(deps), nil
	})
}

// Driver is the kubehz space driver. Implements driver.Driver.
type Driver struct {
	deps *driver.Deps
	// Out is the driver's stdout (bash: the echo lines of provision/destroy).
	// Defaults to os.Stdout.
	Out io.Writer
	// Lib overrides the platform library (tests inject an httptest-bound
	// Context); nil builds one over deps.
	Lib *platform.Context
}

// New builds the driver over its dispatch-provided dependencies.
func New(deps *driver.Deps) *Driver { return &Driver{deps: deps} }

func (d *Driver) stderr() io.Writer {
	if d.deps.Stderr != nil {
		return d.deps.Stderr
	}
	return os.Stderr
}

func (d *Driver) out() io.Writer {
	if d.Out != nil {
		return d.Out
	}
	return os.Stdout
}

func (d *Driver) lib(out io.Writer) *platform.Context {
	if d.Lib != nil {
		c := *d.Lib
		c.Out = out
		return &c
	}
	return &platform.Context{Paths: d.deps.Paths, Runner: d.deps.Runner, Out: out, ErrOut: d.stderr()}
}

// ensureSharedConfig ports driver::ensure_shared_config: every contract
// function needs the same preconditions, and a clear error beats a curl
// failure three calls deep. The read is guarded — an unread config must not
// reach validate as an empty hosting.
func (d *Driver) ensureSharedConfig(c *platform.Context, domain string) (*platform.Config, string, error) {
	cy := filepath.Join(d.deps.Paths.Clusters, domain, "cluster.lok8s.yaml")
	if info, err := os.Stat(cy); err != nil || info.IsDir() {
		ui.Errorf(d.stderr(), "No cluster.lok8s.yaml for domain: %s", domain)
		return nil, "", platform.ErrHandled
	}
	cfg, err := c.ReadConfig(cy)
	if err != nil {
		return nil, "", err
	}
	if err := c.Validate(cfg, cy); err != nil {
		return nil, "", err
	}
	if cfg.Hosting != "shared" {
		ui.Errorf(d.stderr(), "kind: Kubehz requires spec.kubehz.hosting: shared (got: %s)", cfg.Hosting)
		return nil, "", platform.ErrHandled
	}
	return cfg, cy, nil
}

// Provision creates/adopts the space + mints node join tickets. Returns
// driver.ErrFullLifecycle (bash rc 100): a Space has no bootstrap plane, no
// kubeconfig file and no addons — the dispatch tail must not run.
func (d *Driver) Provision(ctx context.Context, domain string) error {
	c := d.lib(d.out())
	cfg, cy, err := d.ensureSharedConfig(c, domain)
	if err != nil {
		return err
	}
	if err := c.ProvisionShared(ctx, cfg, domain, cy); err != nil {
		return err
	}
	return driver.ErrFullLifecycle
}

// Destroy removes the space from kubehz.
func (d *Driver) Destroy(ctx context.Context, domain string) error {
	c := d.lib(d.out())
	cfg, cy, err := d.ensureSharedConfig(c, domain)
	if err != nil {
		return err
	}
	return c.DestroyShared(ctx, cfg, domain, cy)
}

// Status renders the space + node status (the dispatch prints it).
func (d *Driver) Status(ctx context.Context, domain string) (string, error) {
	var buf bytes.Buffer
	c := d.lib(&buf)
	cfg, cy, err := d.ensureSharedConfig(c, domain)
	if err != nil {
		return "", err
	}
	if err := c.SpaceStatus(ctx, cfg, domain, cy); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// Kubeconfig explains how space access works — there is no download.
func (d *Driver) Kubeconfig(ctx context.Context, domain string) (string, error) {
	ui.Errorf(d.stderr(), "A space has no downloadable kubeconfig — the control plane is platform-operated.")
	io.WriteString(d.stderr(), "  Access your namespaces with your kubehz account (OIDC): the dashboard's\n")
	io.WriteString(d.stderr(), "  space page provides a ready-made kubeconfig snippet for 'kubectl oidc-login'.\n")
	return "", errors.New("kubehz: a space has no downloadable kubeconfig")
}
