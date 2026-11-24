package build

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/toolchain"
	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

// noMintCapability is the line that `Secret --capabilities` prints when
// the plugin has the no-mint run (secret.NoMintEnv).
const noMintCapability = "no-mint"

// checkNoMintPlugin is CheckNoMintPlugin for `lo build` (Artifacts). A
// build that reads no store (--no-secrets) does not call it. kustomize
// gets the home that lo checked (o.pluginHome).
func checkNoMintPlugin(ctx context.Context, o *Options, stderr io.Writer) error {
	home, err := CheckNoMintPlugin(ctx, o.Paths, o.runner(), stderr, "lo build")
	if home != "" {
		o.pluginHome = home
	}
	return err
}

// CheckNoMintPlugin refuses a Secret plugin that does not have the no-mint
// run when the process environment asks for it (bash:
// build::_no_mint_capable). Each caller that runs the plugin against a
// store calls it before the run: `lo build`, the addon render (lo
// bootstrap, the KubeOne addon staging, the doctor), `lo k8s` and the
// registry TLS mint. who names the caller at the start of each error line.
// secret.NoMintRequested is true for any value of secret.NoMintEnv that is
// not an off value, an unknown value too. A plugin older than the no-mint
// run ignores the variable and generates values, which is the failure that
// the switch exists to stop. The in-process render serves the plugin from
// this binary, so only the exec pipeline needs the check.
//
// The check is the one of checkKeysOnlyPlugin: the plugin home must
// exist, the Secret executable must be in it, and the probe
// (probeCapability, run by runner, nil = execx.NewRunner(paths)) must
// print "no-mint". Each failure is one line that names the plugin path,
// and the error is ErrHandled. It returns the plugin home that it checked,
// or "" when it did not check (the switch is off, or the render is
// in-process). The home is an absolute path (absPluginHome). The caller gives that home to kustomize, or runs the plugin
// from it, so the run uses the plugin that lo checked.
func CheckNoMintPlugin(ctx context.Context, paths *config.Paths, runner execx.Runner, stderr io.Writer, who string) (string, error) {
	if !secret.NoMintRequested(os.LookupEnv) {
		return "", nil
	}
	if mode, err := render.CurrentMode(); err == nil && mode == render.ModeInProcess {
		return "", nil
	}
	if paths == nil {
		ui.ErrorTo(stderr, "%s: %s is set, but there is no project to find the kustomize plugin home in, so lo cannot check the Secret plugin", who, secret.NoMintEnv)
		return "", ErrHandled
	}
	if runner == nil {
		runner = execx.NewRunner(paths)
	}
	home, err := absPluginHome(paths)
	if err != nil {
		ui.ErrorTo(stderr, "%s: %v", who, err)
		return "", ErrHandled
	}
	if !fsutil.DirExists(home) {
		ui.ErrorTo(stderr, "%s: %s is set, but the kustomize plugin home %s does not exist, so kustomize would load plugins from other directories that lo does not check: run 'lo toolchain install' to install the plugin of this lo",
			who, secret.NoMintEnv, config.RelTo(paths.Base, home))
		return "", ErrHandled
	}
	plugin := filepath.Join(home, filepath.FromSlash(toolchain.SecretPluginRel))
	rel := config.RelTo(paths.Base, plugin)
	if !fsutil.FileExists(plugin) {
		ui.ErrorTo(stderr, "%s: %s is set, but the Secret plugin %s does not exist, so kustomize could load another form of the plugin (a Go plugin, Secret.so) that lo does not check: run 'lo toolchain install' to install the plugin of this lo",
			who, secret.NoMintEnv, rel)
		return "", ErrHandled
	}
	dir, err := os.MkdirTemp("", "lok8s-no-mint-")
	if err != nil {
		ui.ErrorTo(stderr, "%s: cannot make a temporary directory for the check of the Secret plugin %s: %v", who, rel, err)
		return "", ErrHandled
	}
	defer func() { _ = os.RemoveAll(dir) }()
	switch probeCapability(ctx, runner, plugin, dir, noMintCapability) {
	case capabilityFound:
		return home, nil
	case capabilityInterrupted:
		ui.ErrorTo(stderr, "%s: the check of the Secret plugin %s was interrupted", who, rel)
		return "", ErrHandled
	}
	ui.ErrorTo(stderr, "the Secret plugin %s does not have the no-mint run (%s is set, and an older plugin ignores it and generates values): run 'lo toolchain install' to install the plugin of this lo",
		rel, secret.NoMintEnv)
	return "", ErrHandled
}
