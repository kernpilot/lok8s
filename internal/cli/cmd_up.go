package cli

// lo up — the convenience orchestrator: run header → provision dispatch →
// Tilt (`tilt ci` headless, or backgrounded `tilt up` + optional browser).
// Go port of main::up + _lo_header (.lok8s/lo).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/tilt"
)

func init() { registerPorted("up", newUpCommand) }

// upDeps are `lo up`'s seams (tests fake the dispatch and the tilt
// runner; a real Tilt/kind must never start under test).
type upDeps struct {
	dispatch func(ctx context.Context, domainName string) error
	tilt     *tilt.Context
	// lookPath is `command -v` for the browser openers (nil = exec.LookPath).
	lookPath func(tool string) bool
	// open runs the browser opener (nil = the real exec, output inherited).
	open func(tool, url string) error
}

func newUpCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var (
		openTilt bool
		ci       bool
		timeout  string
	)
	cmd := &cobra.Command{
		Use:          "up",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "too many arguments: %s", args[0])
			}
			d := ambientMainEnv(cmd, paths)
			disp := newDispatcher(cmd, paths)
			deps := upDeps{
				dispatch: func(ctx context.Context, domainName string) error { return disp.Dispatch(ctx, domainName, false) },
				tilt: &tilt.Context{
					Paths:  paths,
					Runner: disp.Runner,
					Out:    cmd.OutOrStdout(),
					ErrOut: cmd.ErrOrStderr(),
					Stdin:  cmd.InOrStdin(),
				},
			}
			return runUp(cmd.Context(), paths, cmd.OutOrStdout(), deps, d, ci, timeout, openTilt)
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&openTilt, "open-tilt", "o", false, "Open Tilt in a browser (interactive mode only)")
	f.BoolVar(&ci, "ci", false, "Headless: build+deploy+wait-ready via `tilt ci`, exit with real status (no TTY/browser)")
	f.StringVarP(&timeout, "timeout", "t", "", "Readiness timeout for --ci (e.g. 300s, 10m); passed to `tilt ci`")
	return argshFlagErrors(cmd)
}

// runUp is main::up after the flag parse.
func runUp(ctx context.Context, paths *config.Paths, out io.Writer, deps upDeps, domainName string, ci bool, timeout string, openTilt bool) error {
	writeRunHeader(out, paths, domainName, os.Getenv("KUBECONFIG"))

	// Reconcile infra first regardless of mode (kind cluster, registries,
	// bootstrap addons) — only the post-reconcile Tilt step differs.
	if err := deps.dispatch(ctx, domainName); err != nil {
		return dispatchExit(err)
	}

	// Headless mode: foreground `tilt ci` (build + deploy + wait + exit
	// status). No backgrounded `tilt up`, no browser. Its exit code is
	// main::up's.
	if ci {
		rc, err := deps.tilt.CI(ctx, timeout)
		if err != nil {
			return tiltRun(err)
		}
		if rc != 0 {
			osExit(rc)
			return ErrHandled
		}
		return nil
	}

	if err := deps.tilt.Up(ctx); err != nil {
		return tiltRun(err)
	}
	if openTilt {
		port := deps.tilt.Port()
		url := "http://localhost:" + port
		lookPath := deps.lookPath
		if lookPath == nil {
			lookPath = func(tool string) bool { _, err := exec.LookPath(tool); return err == nil }
		}
		open := deps.open
		if open == nil {
			open = func(tool, url string) error {
				c := exec.Command(tool, url)
				c.Stdout, c.Stderr = os.Stdout, os.Stderr
				return c.Run()
			}
		}
		switch {
		case lookPath("xdg-open"):
			_ = open("xdg-open", url)
		case lookPath("open"):
			_ = open("open", url)
		default:
			fmt.Fprintf(out, "Tilt UI: %s\n", url)
		}
	}
	return nil
}

// writeRunHeader is _lo_header — the compact run header, replacing the bare
// kubeconfig path that used to leak from the driver:
//
//	kubehz-dev  lo · kind · v1.31.12
//	kubeconfig  .kubeconfig/kubehz-dev.yaml
//	registries  local · tls · 3
//
// The registry summary (kind clusters only) reads mode/scheme from the SPEC
// with yq's `//` semantics — see yqScalar for the `tls: false` quirk — and
// the count from the last-generated .registries.json.
func writeRunHeader(out io.Writer, paths *config.Paths, domainName, kubeconfig string) {
	spec := filepath.Join(paths.Clusters, domainName, "cluster.lok8s.yaml")
	deploySpec := filepath.Join(paths.Clusters, domainName, "deploy.lok8s.yaml")
	meta, kind := "", ""
	if fileExists(spec) {
		if k, err := domain.SpecDriver(spec, ""); err == nil {
			kind = k
		}
		meta = kind
		if kind == "lo" {
			meta += " · kind"
		}
		if ver := yqScalar(spec, "", "spec", "kubernetes", "version"); ver != "" && ver != "null" {
			meta += " · " + strings.SplitN(ver, "@", 2)[0]
		}
	} else if fileExists(deploySpec) {
		meta = "deploy → " + yqScalar(deploySpec, "?", "spec", "clusterRef", "domain")
	}
	fmt.Fprintf(out, "\n  \033[1;36m%s\033[0m  \033[2m%s\033[0m\n", domainName, meta)
	fmt.Fprintf(out, "  \033[2mkubeconfig  %s\033[0m\n", strings.TrimPrefix(kubeconfig, paths.Base+"/"))

	if kind == "lo" {
		rMode, rSec, rCount := "local", "plain", "—"
		if yqScalar(spec, "false", "spec", "registries", "shared", "enabled") == "true" {
			rMode = "shared"
		}
		if yqScalar(spec, "true", "spec", "registries", "tls") == "true" {
			rSec = "tls"
		}
		if raw, err := os.ReadFile(filepath.Join(paths.Clusters, domainName, ".registries.json")); err == nil {
			// jq: `.registries | length` — null → 0; a non-JSON file → "—".
			var doc struct {
				Registries []json.RawMessage `json:"registries"`
			}
			if json.Unmarshal(raw, &doc) == nil {
				rCount = fmt.Sprint(len(doc.Registries))
			}
		}
		fmt.Fprintf(out, "  \033[2mregistries  %s · %s · %s\033[0m\n", rMode, rSec, rCount)
	}
	fmt.Fprintln(out)
}
