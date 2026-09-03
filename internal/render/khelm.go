package render

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/mgoltzsche/khelm/v2/pkg/config"
	"github.com/mgoltzsche/khelm/v2/pkg/helm"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// The khelm pin. github.com/mgoltzsche/khelm/v2 v2.8.0 is the SAME release
// the repo pins as the ChartRenderer binary (.bin/b.yaml; kubehz-cluster
// pins v2.8.0 explicitly — "the pair PROVEN to reproduce the committed
// artifacts byte-for-byte"). Its go.mod requires helm.sh/helm/v3 v3.21.2,
// which the root go.mod pins as well: the chart inflation is the same
// helm code the binary was built from. The version strings only feed the
// "Running khelm …" log line the binary prints (stderr, swallowed by
// kustomize unless the plugin fails).
const (
	khelmVersion = "2.8.0"
	helmVersion  = "3.21.2"
)

// khelm's environment contract (cmd/khelm/root.go at v2.8.0).
const (
	envKustomizePluginConfig = "KUSTOMIZE_PLUGIN_CONFIG_STRING"
	envTrustAnyRepo          = "KHELM_TRUST_ANY_REPO"
	envKhelmDebug            = "KHELM_DEBUG"
	envHelmDebug             = "HELM_DEBUG"
	flagTrustAnyRepo         = "trust-any-repo"
)

// runChartRenderer is khelm v2.8.0's kustomize-plugin mode
// (cmd/khelm: Execute → runAsKustomizePlugin), as a library call:
//
//  1. helm.NewHelm() — helm's cli.New() settings (HELM_* env, the same
//     repository cache under $HELM_REPOSITORY_CACHE / XDG cache the binary
//     used, so chart downloads hit the same cache), Debug from
//     KHELM_DEBUG/HELM_DEBUG, TrustAnyRepository from KHELM_TRUST_ANY_REPO.
//  2. The generator config from KUSTOMIZE_PLUGIN_CONFIG_STRING (kustomize
//     exports it alongside the argv[1] temp file; the file is the fallback
//     for a config too long for the environment — the binary would have
//     had no plugin mode at all in that case).
//  3. config.ReadGeneratorConfig: strict decode, metadata.name/namespace
//     applied as the release name/namespace, defaults (namespace
//     "default", helm's default kubeVersion, keyring).
//  4. h.Render with the cwd as BaseDir (kustomize runs the plugin in the
//     kustomization root), cancelled on SIGINT/SIGTERM.
//  5. The resources written as khelm's internal/output.Marshal does: the
//     kyaml encoder (2-space indent), one Encode per RNode document, Close.
//     That package is internal to khelm, so the four lines are repeated
//     here verbatim.
func runChartRenderer(args []string, stdout, stderr io.Writer) error {
	log.SetFlags(0)
	log.SetOutput(stderr)
	debug, _ := strconv.ParseBool(os.Getenv(envKhelmDebug))
	helmDebug, _ := strconv.ParseBool(os.Getenv(envHelmDebug))
	h := helm.NewHelm()
	h.Settings.Debug = debug || helmDebug
	if trustAnyRepo, ok := os.LookupEnv(envTrustAnyRepo); ok {
		trust, _ := strconv.ParseBool(trustAnyRepo)
		h.TrustAnyRepository = &trust
	}

	generatorYAML, ok := os.LookupEnv(envKustomizePluginConfig)
	if !ok {
		if len(args) < 2 || args[1] == "" {
			return errors.New("no generator config: " + envKustomizePluginConfig + " is unset and no config file argument was given")
		}
		raw, err := os.ReadFile(args[1])
		if err != nil {
			return fmt.Errorf("read generator config %s: %w", args[1], err)
		}
		generatorYAML = string(raw)
	}

	log.Println("Running khelm", fmt.Sprintf("%s (helm %s)", khelmVersion, helmVersion))

	req, err := config.ReadGeneratorConfig(strings.NewReader(generatorYAML))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	resources, err := h.Render(ctx, &req.ChartConfig)
	if helm.IsUntrustedRepository(err) {
		log.Printf("HINT: access to untrusted repositories can be enabled using env var %s=true or option --%s", envTrustAnyRepo, flagTrustAnyRepo)
	}
	if err != nil {
		return err
	}
	return marshalResources(resources, stdout)
}

// marshalResources is khelm's internal/output.Marshal.
func marshalResources(resources []*yaml.RNode, w io.Writer) error {
	enc := yaml.NewEncoder(w)
	for i, r := range resources {
		if err := enc.Encode(r.Document()); err != nil {
			return fmt.Errorf("marshal resource %d: %s", i, err)
		}
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("close marshaller: %w", err)
	}
	return nil
}
