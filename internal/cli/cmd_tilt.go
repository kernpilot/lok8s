package cli

// lo tilt — Tilt lifecycle management. Go port of .lok8s/libs/tilt
// (main::tilt + tilt::*); the lifecycle itself lives in internal/tilt.
//
// This file also carries ambientMainEnv, the shared re-creation of the argsh
// entrypoint's pre-dispatch exports (DEBUG, LOK8S_FORCE_RECREATE,
// DOMAIN_NAME, KIND_EXPERIMENTAL_DOCKER_NETWORK, KUBECONFIG) that the
// tilt/image/env/hooks ports all rely on.

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/tilt"
)

func init() { registerPorted("tilt", newTiltCommand) }

// ambientMainEnv replays the bash entrypoint's pre-dispatch environment
// (lo main()) for a ported subcommand and returns the resolved domain:
//
//	-v → DEBUG=1; --force/--force-recreate → LOK8S_FORCE_RECREATE=1;
//	-r → LOK8S_REMOTE=1; DOMAIN_NAME = resolved domain (exported — bare
//	envsubst and tilt::port read it); LOK8S_DOMAIN_EXPLICIT;
//	KIND_EXPERIMENTAL_DOCKER_NETWORK (spec.network.name > env > lok8s);
//	KUBECONFIG = the ambient .kubeconfig/<cluster>.yaml.
func ambientMainEnv(cmd *cobra.Command, paths *config.Paths) string {
	verbose, _ := cmd.Flags().GetCount("verbose")
	force, _ := cmd.Flags().GetBool("force")
	forceRecreate, _ := cmd.Flags().GetBool("force-recreate")
	remote, _ := cmd.Flags().GetBool("remote")
	domainFlag, domainChanged := "", false
	if f := cmd.Flags().Lookup("domain"); f != nil && f.Changed {
		domainFlag, domainChanged = f.Value.String(), true
	}
	clusterFlag := ""
	if f := cmd.Flags().Lookup("cluster"); f != nil && f.Changed {
		clusterFlag = f.Value.String()
	}
	return applyMainEnv(paths, cmd.ErrOrStderr(), verbose > 0,
		force || forceRecreate, remote, domainFlag, domainChanged, clusterFlag)
}

// applyMainEnv performs the exports themselves — shared with the
// hand-parsed subcommands, which extract the globals without cobra.
func applyMainEnv(paths *config.Paths, errOut io.Writer, verbose, forceRecreate, remote bool, domainFlag string, domainChanged bool, clusterFlag string) string {
	if verbose {
		os.Setenv("DEBUG", "1")
	}
	if forceRecreate {
		os.Setenv("LOK8S_FORCE_RECREATE", "1")
	}
	if remote {
		os.Setenv("LOK8S_REMOTE", "1")
	}

	d := domain.Resolve(domainFlag, paths.Clusters, errOut)
	os.Setenv("DOMAIN_NAME", d)
	if domainChanged {
		os.Setenv("LOK8S_DOMAIN_EXPLICIT", "1")
	} else {
		os.Setenv("LOK8S_DOMAIN_EXPLICIT", "0")
	}

	if os.Getenv("KIND_EXPERIMENTAL_DOCKER_NETWORK") == "" {
		os.Setenv("KIND_EXPERIMENTAL_DOCKER_NETWORK", "lok8s")
	}
	if name := specNetworkName(filepath.Join(paths.Clusters, d, "cluster.lok8s.yaml")); name != "" {
		os.Setenv("KIND_EXPERIMENTAL_DOCKER_NETWORK", name)
	}

	os.Setenv("KUBECONFIG", build.AmbientKubeconfig(paths, d, clusterFlag))
	return d
}

// specNetworkName reads .spec.network.name ("" when missing/unreadable).
func specNetworkName(specPath string) string {
	var doc struct {
		Spec struct {
			Network struct {
				Name string `yaml:"name"`
			} `yaml:"network"`
		} `yaml:"spec"`
	}
	raw, err := os.ReadFile(specPath)
	if err != nil || yaml.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return doc.Spec.Network.Name
}

// tiltContext binds a tilt.Context to the command's streams.
func tiltContext(cmd *cobra.Command, paths *config.Paths) *tilt.Context {
	return &tilt.Context{
		Paths:  paths,
		Runner: execx.NewRunner(paths),
		Out:    cmd.OutOrStdout(),
		ErrOut: cmd.ErrOrStderr(),
		Stdin:  cmd.InOrStdin(),
	}
}

// tiltRun maps the tilt package's already-printed sentinel onto the cli one.
func tiltRun(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, tilt.ErrHandled) {
		return ErrHandled
	}
	return err
}

func newTiltCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "tilt",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "Invalid command: %s", args[0])
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newTiltUp(paths),
		newTiltCI(paths),
		newTiltDown(paths),
		newTiltStatus(paths),
		newTiltRestart(paths),
		newTiltPreflight(paths),
	)
	return cmd
}

func newTiltUp(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:          "up",
		Aliases:      []string{"u"},
		Short:        "Spin up tilt (interactive, backgrounded)",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ambientMainEnv(cmd, paths)
			return tiltRun(tiltContext(cmd, paths).Up(cmd.Context()))
		},
	}
}

func newTiltCI(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "ci",
		Short:        "Headless build+deploy+wait-ready (tilt ci), exits with real status",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ambientMainEnv(cmd, paths)
			timeout, _ := cmd.Flags().GetString("timeout")
			rc, err := tiltContext(cmd, paths).CI(cmd.Context(), timeout)
			if err != nil {
				return tiltRun(err)
			}
			if rc != 0 {
				// tilt ci's own exit status IS the contract ("exits with real
				// status") — pass it through instead of collapsing to 1.
				os.Exit(rc)
			}
			return nil
		},
	}
	c.Flags().StringP("timeout", "t", "", "Max time to wait for readiness (e.g. 300s, 10m); passed to `tilt ci`")
	return c
}

func newTiltDown(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:          "down",
		Aliases:      []string{"d"},
		Short:        "Spin down tilt",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ambientMainEnv(cmd, paths)
			force, _ := cmd.Flags().GetBool("force")
			return tiltRun(tiltContext(cmd, paths).Down(cmd.Context(), force))
		},
	}
}

func newTiltStatus(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:          "status",
		Aliases:      []string{"s"},
		Short:        "Check tilt status",
		Annotations:  commandSpec{readonly: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ambientMainEnv(cmd, paths)
			if rc := tiltContext(cmd, paths).Status(cmd.Context()); rc != 0 {
				os.Exit(rc) // `tilt doctor` rc passthrough
			}
			return nil
		},
	}
}

func newTiltRestart(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:          "restart",
		Aliases:      []string{"r"},
		Short:        "Restart tilt",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ambientMainEnv(cmd, paths)
			force, _ := cmd.Flags().GetBool("force")
			return tiltRun(tiltContext(cmd, paths).Restart(cmd.Context(), force))
		},
	}
}

func newTiltPreflight(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "preflight",
		Short:        "Force-clear stuck-Terminating objects in the manifest on stdin",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ambientMainEnv(cmd, paths)
			age, _ := cmd.Flags().GetString("age")
			crds, _ := cmd.Flags().GetString("crds")
			crdAllow, _ := cmd.Flags().GetString("crd-allow")
			domainFlag := ""
			if f := cmd.Flags().Lookup("domain"); f != nil && f.Changed {
				domainFlag = f.Value.String()
			}
			return tiltRun(tiltContext(cmd, paths).Preflight(cmd.Context(), domainFlag, age, crds, crdAllow))
		},
	}
	c.Flags().StringP("age", "a", "", "Only clear objects terminating longer than this many seconds (default 30)")
	c.Flags().StringP("crds", "c", "", "Stuck-CRD policy: drain (clear instance finalizers), skip, or force")
	c.Flags().String("crd-allow", "", "Comma-separated CRD names the force policy may strip (empty = all)")
	return c
}
