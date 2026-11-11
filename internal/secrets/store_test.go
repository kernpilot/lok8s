package secrets

// store_test.go: `lo secrets` for a domain whose directory exists without a
// store, in a project that keeps a store per domain, does not fall back to
// the flat .secrets/ store (bash: a failing secrets::path). `set` creates the
// store first, so it never stops. A project with only the flat store keeps
// working.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/fsutil"
)

func TestResolveStoreRefusesADomainWithoutAStore(t *testing.T) {
	c, out, errOut := testEnv(t)
	write(t, c.Paths.Clusters+"/a.dev/secrets/Secret.app.default.K", "a")
	write(t, c.Paths.Base+"/.secrets/Secret.app.default.K", "flat")
	if err := os.MkdirAll(c.Paths.Clusters+"/new.dev", 0o755); err != nil {
		t.Fatal(err)
	}
	c.Domain = "new.dev"

	want := "[error] domain new.dev has no secrets store: " + c.Paths.Clusters + "/new.dev/secrets does not exist"
	for name, run := range map[string]func() error{
		"path":    func() error { _, err := c.ResolveStore(); return err },
		"list":    c.List,
		"decrypt": func() error { return c.Decrypt("/nonexistent-ssh-key") },
		"encrypt": func() error { return c.Encrypt("") },
		"env":     func() error { return c.Env("app", "default") },
		"allow":   c.Allow,
	} {
		out.Reset()
		errOut.Reset()
		if err := run(); !errors.Is(err, ErrHandled) {
			t.Errorf("%s: err = %v, want ErrHandled", name, err)
		}
		if !strings.HasPrefix(errOut.String(), want) || out.Len() != 0 {
			t.Errorf("%s: stdout %q stderr %q", name, out.String(), errOut.String())
		}
	}

	// set scaffolds the store first (the domain directory exists): no stop,
	// and the value lands in the new store, not the flat one.
	errOut.Reset()
	if err := c.Set(context.Background(), "app", "default", "K", "v", false); err != nil {
		t.Fatalf("set: %v (%s)", err, errOut.String())
	}
	if !fsutil.IsRegular(c.Paths.Clusters + "/new.dev/secrets/Secret.app.default.K") {
		t.Error("set did not write the new store")
	}
	if raw, _ := os.ReadFile(c.Paths.Base + "/.secrets/Secret.app.default.K"); string(raw) != "flat" {
		t.Errorf("flat store changed: %q", raw)
	}
	if dir, err := c.ResolveStore(); err != nil || dir != c.Paths.Clusters+"/new.dev/secrets" {
		t.Errorf("after set: %q, %v", dir, err)
	}
}

// A project with only the flat store (no clusters/*/secrets) keeps using it.
func TestResolveStoreLegacyFlatProject(t *testing.T) {
	c, _, errOut := testEnv(t)
	write(t, c.Paths.Base+"/.secrets/Secret.app.default.K", "flat")
	if err := os.MkdirAll(c.Paths.Clusters+"/only.dev", 0o755); err != nil {
		t.Fatal(err)
	}
	c.Domain = "only.dev"
	dir, err := c.ResolveStore()
	if err != nil || dir != c.Paths.Base+"/.secrets" || errOut.Len() != 0 {
		t.Errorf("ResolveStore = %q, %v, stderr %q", dir, err, errOut.String())
	}
}
