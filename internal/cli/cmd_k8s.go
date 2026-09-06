package cli

// lo k8s — K8s artifact generation (hidden; legacy capigen-era paths). Go
// port of .lok8s/libs/k8s (main::k8s): `capi` renders CAPI resources through
// the capi driver's template generator (internal/driver/capi — the port of
// capi::generate), `infrastructure` / `platform` kustomize-build the domain's
// targets / legacy overlay into artifacts. Ported faithfully but minimal.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/driver/capi"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() { registerPorted("k8s", newK8sCommand) }

func newK8sCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "k8s",
		Aliases:      spec.aliases,
		Short:        spec.short,
		Hidden:       spec.hidden,
		Annotations:  spec.annotations(),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "Invalid command: %s", args[0])
			}
			return cmd.Help()
		},
	}

	// The domain: --domain flag > DOMAIN_NAME > clusters/.active > lok8s.dev
	// (the entrypoint's domain::resolve, inherited by every k8s:: function).
	resolveDomain := func(cmd *cobra.Command) string {
		setDebugFromVerbose(cmd)
		flag, _ := cmd.Flags().GetString("domain")
		return domain.Resolve(flag, paths.Clusters, cmd.ErrOrStderr())
	}

	var specPath, outPath string
	capiCmd := &cobra.Command{
		Use: "capi", Aliases: []string{"c"},
		Short:        "Generate CAPI resources from cluster spec",
		Annotations:  map[string]string{AnnotationIdempotent: "true"},
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return k8sCapi(paths, resolveDomain(cmd), specPath, outPath, cmd.ErrOrStderr())
		},
	}
	capiCmd.Flags().StringVar(&specPath, "spec", "", "Path to cluster.lok8s.yaml spec")
	capiCmd.Flags().StringVar(&outPath, "out", "", "Output file (default: .lok8s/<domain>/artifacts/capi.yaml)")

	cmd.AddCommand(
		capiCmd,
		&cobra.Command{
			Use: "infrastructure", Aliases: []string{"i"},
			Short:        "Build infrastructure artifacts",
			Annotations:  map[string]string{AnnotationIdempotent: "true"},
			Args:         cobra.NoArgs,
			SilenceUsage: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				d := resolveDomain(cmd)
				outDir := paths.Clusters + "/" + d + "/artifacts"
				return k8sKustomizeArtifact(paths, d, paths.Clusters+"/"+d+"/targets", outDir, "infrastructure.yaml", cmd.ErrOrStderr())
			},
		},
		&cobra.Command{
			Use:          "platform",
			Short:        "Build platform artifacts",
			Annotations:  map[string]string{AnnotationIdempotent: "true"},
			Args:         cobra.NoArgs,
			SilenceUsage: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				d := resolveDomain(cmd)
				stderr := cmd.ErrOrStderr()
				ui.Warnf(stderr, "k8s::platform uses legacy overlay paths; consider migrating to targets/")
				overlay := paths.Base + "/.k8s/overlays/" + d
				if info, err := os.Stat(overlay); err != nil || !info.IsDir() {
					ui.Errorf(stderr, "Overlay not found: %s", overlay)
					return ErrHandled
				}
				return k8sKustomizeArtifact(paths, d, overlay, overlay+"/artifacts", "platform.yaml", stderr)
			},
		},
	)
	return cmd
}

// k8sCapi is k8s::capi: resolve the spec (explicit --spec, else the domain's
// cluster.lok8s.yaml, else the legacy <base>/<domain>.lok8s.yaml), render
// the CAPI resources for the detected provider into --out (default: the
// SPEC's domain artifacts dir — anchored to the cluster the spec describes,
// not the --domain flag), and drop a kustomization.yaml next to it when
// absent.
func k8sCapi(paths *config.Paths, d, specPath, outPath string, stderr io.Writer) error {
	if specPath == "" {
		specPath = paths.Clusters + "/" + d + "/cluster.lok8s.yaml"
		if !fileExists(specPath) {
			specPath = paths.Base + "/" + d + ".lok8s.yaml"
		}
	}
	if !fileExists(specPath) {
		ui.Errorf(stderr, "Spec not found: %s", specPath)
		return ErrHandled
	}
	if outPath == "" {
		specDomain := specClusterDomain(specPath)
		if specDomain == "" {
			specDomain = "unknown"
		}
		outPath = paths.Clusters + "/" + specDomain + "/artifacts/capi.yaml"
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}

	drv := capi.New(&driver.Deps{Paths: paths, Runner: execx.NewRunner(paths), Stderr: stderr})
	provider, err := drv.DetectProvider(specPath)
	if err != nil {
		return ErrHandled
	}
	ui.Debugf(stderr, "Generating CAPI resources for provider: %s", provider)
	// bash: `capi::generate … > "${out}"` — the redirect truncates the
	// target before the render runs.
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	rendered, err := drv.Generate(specPath, provider)
	if err != nil {
		_ = f.Close()
		return ErrHandled
	}
	if _, err := f.WriteString(rendered); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := writeKustomizationIfAbsent(filepath.Dir(outPath), filepath.Base(outPath)); err != nil {
		return err
	}
	ui.Debugf(stderr, "Generated CAPI resources: %s", outPath)
	return nil
}

// specClusterDomain reads .spec.cluster.domain ("" when absent).
func specClusterDomain(specPath string) string {
	var doc struct {
		Spec struct {
			Cluster struct {
				Domain string `yaml:"domain"`
			} `yaml:"cluster"`
		} `yaml:"spec"`
	}
	raw, err := os.ReadFile(specPath)
	if err != nil || yaml.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return doc.Spec.Cluster.Domain
}

// k8sKustomizeArtifact is the shared body of k8s::infrastructure /
// k8s::platform: `KUBECONFIG=<base>/.kubeconfig/secret.<domain>.yaml
// kustomize build --enable-alpha-plugins <src> > <outDir>/<file>` (via
// internal/render) plus a
// kustomization.yaml listing the artifact when absent.
func k8sKustomizeArtifact(paths *config.Paths, d, src, outDir, file string, stderr io.Writer) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(outDir + "/" + file)
	if err != nil {
		return err
	}
	// internal/render: in-process by default, the pinned kustomize binary
	// under LO_RENDER=exec. The file is created (truncated) before the
	// render, as the redirect did.
	out, runErr := render.Build(context.Background(), src, render.Options{
		Paths:  paths,
		Env:    []string{"KUBECONFIG=" + paths.Base + "/.kubeconfig/secret." + d + ".yaml"},
		Stderr: stderr,
	})
	if runErr == nil {
		_, runErr = f.Write(out)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if runErr != nil {
		return ErrHandled
	}
	return writeKustomizationIfAbsent(outDir, file)
}

// writeKustomizationIfAbsent drops the one-resource kustomization.yaml the
// bash heredocs wrote next to each artifact.
func writeKustomizationIfAbsent(dir, resource string) error {
	path := dir + "/kustomization.yaml"
	if fileExists(path) {
		return nil
	}
	body := fmt.Sprintf("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - %s\n", resource)
	return os.WriteFile(path, []byte(body), 0o644)
}
