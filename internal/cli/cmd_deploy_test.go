package cli

// cmd_deploy_test.go ports the main::deploy half of
// tests/unit/deploy_test.bats: -l/--label routing + the key=value guard,
// with the two apply entrypoints stubbed (the bats redefine deploy::apply /
// deploy::apply_filtered the same way).

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

type routeRecorder struct{ route string }

func (r *routeRecorder) Apply(ctx context.Context, domain string) error {
	r.route = "route=apply domain=" + domain
	return nil
}

func (r *routeRecorder) ApplyFiltered(ctx context.Context, domain, key, value string) error {
	r.route = "route=filtered domain=" + domain + " key=" + key + " val=" + value
	return nil
}

// bats: "main::deploy -l key=value routes to apply_filtered"
func TestDeployLabelRoutesToFiltered(t *testing.T) {
	r := &routeRecorder{}
	if err := runDeploy(context.Background(), r, &bytes.Buffer{}, "test.lok8s.dev", "lok8s.dev/name=zitadel"); err != nil {
		t.Fatal(err)
	}
	if r.route != "route=filtered domain=test.lok8s.dev key=lok8s.dev/name val=zitadel" {
		t.Errorf("route = %q", r.route)
	}
}

// bats: "main::deploy without -l routes to apply (full artifact)"
func TestDeployNoLabelRoutesToApply(t *testing.T) {
	r := &routeRecorder{}
	if err := runDeploy(context.Background(), r, &bytes.Buffer{}, "test.lok8s.dev", ""); err != nil {
		t.Fatal(err)
	}
	if r.route != "route=apply domain=test.lok8s.dev" {
		t.Errorf("route = %q", r.route)
	}
}

// bats: "main::deploy -l =value / foo / foo= errors expected key=value"
func TestDeployBadLabelErrors(t *testing.T) {
	for _, bad := range []string{"=value", "foo", "foo="} {
		r := &routeRecorder{}
		var errBuf bytes.Buffer
		err := runDeploy(context.Background(), r, &errBuf, "test.lok8s.dev", bad)
		if !errors.Is(err, ErrHandled) {
			t.Errorf("%q: err = %v", bad, err)
		}
		if !strings.Contains(errBuf.String(), "expected key=value") || r.route != "" {
			t.Errorf("%q: stderr=%q route=%q", bad, errBuf.String(), r.route)
		}
	}
}
