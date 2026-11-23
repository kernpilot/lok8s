package build

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/toolchain"
	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

// noMintCapability is the line that `Secret --capabilities` prints when
// the plugin has the no-mint run (secret.NoMintEnv).
const noMintCapability = "no-mint"

// checkNoMintPlugin refuses a Secret plugin that does not have the no-mint
// run when the process environment asks for it (bash:
// build::_no_mint_capable). secret.NoMintRequested is true for any value
// of secret.NoMintEnv that is not an off value, an unknown value too. A
// plugin older than the no-mint run ignores the variable and generates
// values into the store, which is the failure that the switch exists to
// stop. The in-process render serves the plugin from this binary, so
// only the exec pipeline needs the check. A build that reads no store
// (--no-secrets) does not call it.
//
// The check is the one of checkKeysOnlyPlugin: the plugin home must
// exist, the Secret executable must be in it, and the probe (probeCapability)
// must print "no-mint". kustomize gets the home that lo checked
// (o.pluginHome). Each failure is one line that names the plugin path.
func checkNoMintPlugin(ctx context.Context, o *Options, stderr io.Writer) error {
	if !secret.NoMintRequested(os.LookupEnv) {
		return nil
	}
	if mode, err := render.CurrentMode(); err == nil && mode == render.ModeInProcess {
		return nil
	}
	home := config.KustomizePluginHome(o.Paths)
	if !fsutil.DirExists(home) {
		ui.ErrorTo(stderr, "lo build: %s is set, but the kustomize plugin home %s does not exist, so kustomize would load plugins from other directories that lo does not check: run 'lo toolchain install' to install the plugin of this lo",
			secret.NoMintEnv, config.RelTo(o.Paths.Base, home))
		return ErrHandled
	}
	o.pluginHome = home
	plugin := filepath.Join(home, filepath.FromSlash(toolchain.SecretPluginRel))
	rel := config.RelTo(o.Paths.Base, plugin)
	if !fsutil.FileExists(plugin) {
		ui.ErrorTo(stderr, "lo build: %s is set, but the Secret plugin %s does not exist, so kustomize could load another form of the plugin (a Go plugin, Secret.so) that lo does not check: run 'lo toolchain install' to install the plugin of this lo",
			secret.NoMintEnv, rel)
		return ErrHandled
	}
	dir, err := os.MkdirTemp("", "lok8s-no-mint-")
	if err != nil {
		ui.ErrorTo(stderr, "lo build: cannot make a temporary directory for the check of the Secret plugin %s: %v", rel, err)
		return ErrHandled
	}
	defer func() { _ = os.RemoveAll(dir) }()
	switch probeCapability(ctx, o, plugin, dir, noMintCapability) {
	case capabilityFound:
		return nil
	case capabilityInterrupted:
		ui.ErrorTo(stderr, "lo build: the check of the Secret plugin %s was interrupted", rel)
		return ErrHandled
	}
	ui.ErrorTo(stderr, "the Secret plugin %s does not have the no-mint run (%s is set, and an older plugin ignores it and generates values): run 'lo toolchain install' to install the plugin of this lo",
		rel, secret.NoMintEnv)
	return ErrHandled
}
