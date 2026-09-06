package provision

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ProviderLoader resolves a provider name to a loaded provider
// implementation (bash: provider::load sourcing providers/<name>/main and
// checking the contract — in Go the interface IS the contract check). The
// actual hetzner provider port comes later; until then a nil loader means
// "no providers available" and any named provider fails with the bash
// not-found error.
type ProviderLoader interface {
	Load(name string) (driver.Provider, error)
}

// providerNameRe is the provider-name allowlist (bash: provider::read_name).
var providerNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// ReadProviderName reads spec.provider.name with the DISPATCH's semantics:
// the bash call sites run `provider::read_name … 2>/dev/null || true`, so a
// missing OR invalid name both come back as "" (provider silently skipped).
func ReadProviderName(specFile string) string {
	info, err := readSpecInfo(specFile)
	if err != nil {
		return ""
	}
	name := info.Spec.Provider.Name
	if name == "" || !providerNameRe.MatchString(name) {
		return ""
	}
	return name
}

// WriteProviderConfig resolves the provider config to a file path (bash:
// provider::write_config → PROVIDER_CONFIG_FILE). Two modes, mutually
// exclusive:
//
//	spec.provider.configRef → a file relative to the cluster's domain dir
//	                          (used directly, no copy)
//	spec.provider.config    → the inline block, extracted to a temp file
//
// The path is also exported as PROVIDER_CONFIG_FILE (bash parity — drivers
// and provider functions read it). cleanup removes the temp file (nil in
// configRef mode); the bash used a process-exit trap, the Go caller defers.
func WriteProviderConfig(specFile string, stderr io.Writer) (configFile string, cleanup func(), err error) {
	info, _ := readSpecInfo(specFile)

	if ref := info.Spec.Provider.ConfigRef; ref != "" {
		refPath := filepath.Join(filepath.Dir(specFile), ref)
		if !fileExists(refPath) {
			ui.Errorf(stderr, "provider.configRef '%s' not found at %s", ref, refPath)
			return "", nil, fmt.Errorf("provider configRef not found: %s", ref)
		}
		os.Setenv("PROVIDER_CONFIG_FILE", refPath)
		ui.Debugf(stderr, "provider config from configRef: %s", refPath)
		return refPath, nil, nil
	}

	f, err := os.CreateTemp("", "lok8s-provider-config.*.yaml")
	if err != nil {
		return "", nil, err
	}
	content := []byte("{}\n")
	if !info.Spec.Provider.Config.IsZero() {
		if b, merr := yaml.Marshal(&info.Spec.Provider.Config); merr == nil {
			content = b
		}
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, err
	}
	os.Setenv("PROVIDER_CONFIG_FILE", f.Name())
	ui.Debugf(stderr, "provider config written to %s", f.Name())
	name := f.Name()
	return name, func() { _ = os.Remove(name) }, nil
}
