package provision

// testutil_test.go holds the fixtures the provision tests share: the two
// spec fixtures, the fake drivers, the fake provider and loader, and the
// dispatcher over a temp project.

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// loSpecYAML mirrors the lo-cluster fixture the bats copy in.
const loSpecYAML = `apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-cluster
spec:
  kubernetes:
    version: "v1.31.10"
`

const deploySpecYAML = `apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: staging-apps
spec:
  clusterRef:
    domain: test.lok8s.dev
`

// fakeDriver records lifecycle calls; optional-interface variants wrap it.
type fakeDriver struct {
	log          *[]string
	provisionErr error
	destroyErr   error
	statusWord   string
	statusErr    error
}

func (f *fakeDriver) Provision(ctx context.Context, domain string) error {
	*f.log = append(*f.log, "provision:"+domain)
	return f.provisionErr
}

func (f *fakeDriver) Destroy(ctx context.Context, domain string) error {
	*f.log = append(*f.log, "destroy:"+domain)
	return f.destroyErr
}

func (f *fakeDriver) Status(ctx context.Context, domain string) (string, error) {
	*f.log = append(*f.log, "status:"+domain)
	if f.statusWord == "" {
		return "Running", f.statusErr
	}
	return f.statusWord, f.statusErr
}

func (f *fakeDriver) Kubeconfig(ctx context.Context, domain string) (string, error) {
	return "", nil
}

type fakeExportingDriver struct{ *fakeDriver }

func (f fakeExportingDriver) Export(ctx context.Context, domain string) error {
	*f.log = append(*f.log, "export:"+domain)
	return nil
}

type fakePostProvisionDriver struct{ *fakeDriver }

func (f fakePostProvisionDriver) PostProvision(ctx context.Context, domain string) error {
	*f.log = append(*f.log, "post_provision:"+domain)
	return nil
}

// loDispatcher builds a Dispatcher around a test.lok8s.dev lo-driver
// domain, mirroring the bats setup (kubehz/bootstrap hooks stubbed off =
// nil hooks skipped).
func loDispatcher(t *testing.T, drv driver.Driver) (*Dispatcher, *bytes.Buffer) {
	t.Helper()
	p := testPaths(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), loSpecYAML)
	var errBuf bytes.Buffer
	d := &Dispatcher{
		Paths:  p,
		Stderr: &errBuf,
		Stdout: &bytes.Buffer{},
		Drivers: func(name string) (driver.Factory, bool) {
			if name != "lo" || drv == nil {
				return nil, false
			}
			return func(deps *driver.Deps) (driver.Driver, error) { return drv, nil }, true
		},
	}
	return d, &errBuf
}

// fakeProvider is a loaded provider that records nothing and validates.
type fakeProvider struct{ name string }

func (p *fakeProvider) Validate(context.Context, string) error { return nil }

func (p *fakeProvider) CredentialData(context.Context, string) (map[string]string, error) {
	return nil, nil
}

func (p *fakeProvider) Provision(context.Context, string, string) error { return nil }

func (p *fakeProvider) Destroy(context.Context, string, string) error { return nil }

func (p *fakeProvider) Output(context.Context, string) ([]byte, error) {
	return []byte(`{"nodes":[]}`), nil
}

type fakeLoader struct{ loaded []string }

func (l *fakeLoader) Load(_ context.Context, name string) (driver.Provider, error) {
	l.loaded = append(l.loaded, name)
	return &fakeProvider{name: name}, nil
}
