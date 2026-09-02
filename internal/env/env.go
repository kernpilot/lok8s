// Package env is the Go port of .lok8s/libs/env — environment and service
// configuration. Merges services.yaml + services.<config>.yaml and generates
// the Tilt overlay kustomization (clusters/<domain>/artifacts/
// kustomization.yaml). Driven by the Tilt extension via the hidden `lo env`
// command.
//
// The deep-merge itself still shells out to yq (`yq eval-all '. as $item
// ireduce ({}; . * $item )'`) through the Runner seam — `lo env services`
// output is a user-visible stream the Tiltfile consumes, and only the same
// binary guarantees byte parity. Field EXTRACTION from the merged YAML is
// native (ordered yaml.Node walk with yq's `//`-swallows-false semantics).
package env

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrHandled marks a failure whose message was already printed in the bash
// format; callers exit non-zero without printing anything further.
var ErrHandled = errors.New("env: handled")

// Context carries one env-command invocation.
type Context struct {
	Paths  *config.Paths
	Runner execx.Runner
	Out    io.Writer
	ErrOut io.Writer
	// Domain is the resolved active domain (bash: the dynamically scoped
	// `${domain:-${DOMAIN_NAME:-lok8s.dev}}`).
	Domain string

	// BuildArtifacts is the build::artifacts seam (nil → build.Artifacts
	// with the spec-driven split, exactly like `lo build`'s core).
	BuildArtifacts func() error
	// Pull drains the cache queue (bash: image::cache --all) — wired by the
	// CLI layer to the image package, keeping env free of an import cycle.
	Pull func() error
}

// Services prints the merged service configuration (bash: env::services).
// Merges services.yaml (committed) + services.<config>.yaml (override).
// Returns empty YAML ({}) when no services.yaml exists (projects that manage
// k8s manifests directly without the services abstraction).
func (c *Context) Services(ctx context.Context, onlyServices, onlyRegistry bool) error {
	out, err := c.servicesYAML(ctx, onlyServices, onlyRegistry, c.ErrOut)
	if err != nil {
		return err
	}
	_, err = c.Out.Write(out)
	return err
}

// ServicesRaw returns the merged services env as bytes — the capture form
// other libs consume (bash: `merged=$(env::services 2>/dev/null)`; callers
// set ErrOut to io.Discard for the same silencing).
func (c *Context) ServicesRaw(ctx context.Context) ([]byte, error) {
	return c.servicesYAML(ctx, false, false, c.ErrOut)
}

// servicesYAML is the capture form of Services. errOut receives yq's stderr
// and the debug lines (env::kustomization calls it as `env::services
// 2>/dev/null`, i.e. with a discard writer).
func (c *Context) servicesYAML(ctx context.Context, onlyServices, onlyRegistry bool, errOut io.Writer) ([]byte, error) {
	ui.Debugf(errOut, "Print services")

	baseFile := c.Paths.Base + "/services.yaml"
	if !fileExists(baseFile) {
		// Fall back to legacy filenames.
		baseFile = c.Paths.Base + "/services.base.yaml"
		if !fileExists(baseFile) {
			// No services config at all — return empty YAML.
			ui.Debugf(errOut, "No services.yaml found; returning empty config")
			return []byte("{}\n"), nil
		}
	}

	var input strings.Builder
	base, err := os.ReadFile(baseFile)
	if err != nil {
		return nil, err
	}
	input.Write(base)
	// Merge config-specific override if LOK8S_SERVICE_CONFIG is set.
	if cfg := os.Getenv("LOK8S_SERVICE_CONFIG"); cfg != "" {
		override := c.Paths.Base + "/services." + cfg + ".yaml"
		if raw, err := os.ReadFile(override); err == nil {
			input.WriteString("\n---\n\n")
			input.Write(raw)
		}
	}
	// Legacy: also merge services.default.yaml if it exists.
	if raw, err := os.ReadFile(c.Paths.Base + "/services.default.yaml"); err == nil {
		input.WriteString("\n---\n\n")
		input.Write(raw)
	}

	merged, err := c.yq(ctx, input.String(), errOut, "eval-all", ". as $item ireduce ({}; . * $item )")
	if err != nil {
		return nil, err
	}
	if onlyServices {
		merged, err = c.yq(ctx, merged, errOut, ".services")
	} else if onlyRegistry {
		merged, err = c.yq(ctx, merged, errOut, ".registry")
	}
	if err != nil {
		return nil, err
	}
	return BareEnvsubst([]byte(merged)), nil
}

// yq runs one yq invocation with stdin/stdout piped (the merge stage of the
// bash pipeline). stderr goes to errOut, mirroring where the shell left it.
func (c *Context) yq(ctx context.Context, stdin string, errOut io.Writer, args ...string) (string, error) {
	var out strings.Builder
	err := c.Runner.Run(ctx, execx.Cmd{
		Name: "yq", Args: args,
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: errOut,
	})
	return out.String(), err
}

// Kustomization generates clusters/<domain>/artifacts/kustomization.yaml
// (bash: env::kustomization) — the Tilt overlay that wraps the SINGLE domain
// artifact (clusters/<domain>/artifacts.yaml, built by build.Artifacts) and
// layers the services.yaml image swaps on top. Tilt builds this dir into one
// pool it partitions via filter_yaml() (see .lok8s/tilt/Tiltfile). NOTE it
// deliberately writes INTO the artifacts/ directory the split emit
// preserves (env's overlay kustomization.yaml, capi.yaml and .cache-queue
// live as files directly under artifacts/ — see build.Artifacts'
// pruneStaleArtifactDirs).
func (c *Context) Kustomization(ctx context.Context, noBuild, pull bool) error {
	ui.Debugf(c.ErrOut, "Generate kustomization.yaml")

	if !noBuild {
		if err := c.buildArtifacts(); err != nil {
			return err
		}
	}

	// Read merged services env once (bash: `env::services 2>/dev/null`).
	merged, err := c.servicesYAML(ctx, false, false, io.Discard)
	if err != nil {
		return err
	}
	var doc yaml.Node
	_ = yaml.Unmarshal(merged, &doc)

	// Global registry config. Three distinct concepts:
	//   prefix   = canonical local image name (lok8s.local — what manifests
	//              reference, what docker_build produces). Never remote.
	//   cache    = on-cluster cache hostname (lok8s.cache — what build:false
	//              services get swapped to so kind pulls without upstream creds).
	//   endpoint = remote registry target (ghcr.io/org, etc.) — only used
	//              when pre-pulling a non-built service into the cache.
	// branch/tag are the path components of both the remote and the cache ref.
	gPrefix := ScalarOr(&doc, "lok8s.local", "registry", "prefix")
	gCache := "lok8s.cache"
	gEndpoint := subst(ScalarOr(&doc, "${DOCKER_REGISTRY}", "registry", "endpoint"))
	gBranch := subst(ScalarOr(&doc, "${DOCKER_PROJECT}", "registry", "branch"))
	gTag := subst(ScalarOr(&doc, "${DOCKER_TAG}", "registry", "tag"))

	// Default for the per-service `build:` field. Mirrors Tiltfile resolution:
	// per-service > defaults.build > true.
	// NOTE: yq's `//` operator is "alternative on null OR false" — it
	// swallows legitimate `false` values. The only reliable way to
	// distinguish "missing" from "present and false" is `| tostring`,
	// which emits "null" for missing keys and "false"/"true" for bools.
	defaultBuild := ToString(&doc, "defaults", "build")
	if defaultBuild == "null" {
		defaultBuild = "true"
	}

	// Cache pre-pull queue: services that need a remote image fetched and
	// pushed into the cache registry before kind tries to pull them.
	// (TSV: svc \t remote \t branch \t tag).
	artifactsDir := c.Paths.Clusters + "/" + c.Domain + "/artifacts"
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		return err
	}
	cacheQueue := artifactsDir + "/.cache-queue"
	if err := os.WriteFile(cacheQueue, nil, 0o644); err != nil {
		return err
	}

	images, err := c.generateImages(&doc, defaultBuild, gPrefix, gCache, gEndpoint, gBranch, gTag, cacheQueue)
	if err != nil {
		return err
	}

	outFile := artifactsDir + "/kustomization.yaml"
	domainArtifact := c.Paths.Clusters + "/" + c.Domain + "/artifacts.yaml"

	var buf strings.Builder
	buf.WriteString("# Auto-generated by lok8s — do not edit. Source: lo env kustomization.\n")
	buf.WriteString("apiVersion: kustomize.config.k8s.io/v1beta1\n")
	buf.WriteString("kind: Kustomization\n")
	buf.WriteString("\n")
	if fileExists(domainArtifact) {
		// ../artifacts.yaml is a raw file above the overlay root — Tilt
		// builds this dir with --load-restrictor=LoadRestrictionsNone so the
		// reference resolves.
		buf.WriteString("resources:\n")
		buf.WriteString("  - ../artifacts.yaml\n")
	} else {
		buf.WriteString("resources: []\n")
	}
	if images != "" {
		buf.WriteString("\n")
		buf.WriteString("images:\n")
		buf.WriteString(images + "\n")
	}
	if err := os.WriteFile(outFile, []byte(buf.String()), 0o644); err != nil {
		return err
	}

	// Cache pre-pull is opt-in via --pull. Tilt and lo build orchestrate the
	// cache step separately so failures surface in the right layer and users
	// can run kustomization generation without forcing a network round trip.
	// The queue file is always written; consumers drain it when ready.
	if pull {
		if c.Pull == nil {
			// Unreachable through the CLI (which always wires Pull); kept for
			// the bash contract's sake.
			ui.Errorf(c.ErrOut, "--pull requires the image lib (run via 'lo env kustomization --pull', not by sourcing env standalone)")
			return ErrHandled
		}
		if info, err := os.Stat(cacheQueue); err == nil && info.Size() > 0 {
			raw, _ := os.ReadFile(cacheQueue)
			ui.Debugf(c.ErrOut, "draining %d cache queue entries", strings.Count(string(raw), "\n"))
			if err := c.Pull(); err != nil {
				ui.Errorf(c.ErrOut, "image::cache --all failed; check upstream credentials and network")
				return ErrHandled
			}
		}
	}
	return nil
}

func (c *Context) buildArtifacts() error {
	if c.BuildArtifacts != nil {
		return c.BuildArtifacts()
	}
	return build.Artifacts(build.Options{
		Paths:         c.Paths,
		Domain:        c.Domain,
		SplitOverride: os.Getenv("LOK8S_BUILD_SPLIT"),
		NoSecrets:     build.NoSecretsEffective(false),
		Stderr:        c.ErrOut,
	})
}

// generateImages generates the image override lines for kustomization.yaml
// from the merged services config (bash: env::generate_images). Also writes
// the cache queue entries as a side effect.
func (c *Context) generateImages(doc *yaml.Node, defaultBuild, gPrefix, gCache, gEndpoint, gBranch, gTag, cacheQueue string) (string, error) {
	services := Path(doc, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return "", nil
	}

	queue, err := os.OpenFile(cacheQueue, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer queue.Close()

	var out strings.Builder
	for i := 0; i+1 < len(services.Content); i += 2 {
		svc := services.Content[i].Value
		if svc == "" {
			continue
		}
		svcNode := deref(services.Content[i+1])

		// Skip disabled services (tostring-sentinel because // swallows false).
		enabled := toStringNode(mapGet(svcNode, "enabled"))
		if enabled == "null" {
			enabled = "true"
		}
		if enabled == "false" {
			continue
		}

		// Resolve effective build (per-service > defaults > true).
		svcBuild := toStringNode(mapGet(svcNode, "build"))
		if svcBuild == "null" {
			svcBuild = defaultBuild
		}

		// Pinned image (mutually exclusive with registry per validator).
		pinned := scalarOrNode(mapGet(svcNode, "image"), "")
		if pinned != "" {
			if strings.Contains(pinned, "@sha256:") {
				// bash: ${pinned%@*} / ${pinned##*@} — split on the LAST @.
				at := strings.LastIndex(pinned, "@")
				fmt.Fprintf(&out, "  - name: %s/%s\n    newName: %s\n    digest: \"%s\"\n", gPrefix, svc, pinned[:at], pinned[at+1:])
			} else if colon := strings.LastIndex(pinned, ":"); colon >= 0 {
				fmt.Fprintf(&out, "  - name: %s/%s\n    newName: %s\n    newTag: \"%s\"\n", gPrefix, svc, pinned[:colon], pinned[colon+1:])
			} else {
				fmt.Fprintf(&out, "  - name: %s/%s\n    newName: %s\n", gPrefix, svc, pinned)
			}
			continue
		}

		// No pin: only emit a swap if we're NOT building locally.
		if svcBuild == "true" {
			continue
		}

		// build:false branch on registry-presence:
		//   per-service.registry.endpoint > global.registry.endpoint > skip+warn
		reg := mapGet(svcNode, "registry")
		sEndpoint := subst(scalarOrNode(mapGet(reg, "endpoint"), gEndpoint))
		sBranch := subst(scalarOrNode(mapGet(reg, "branch"), gBranch))
		sTag := subst(scalarOrNode(mapGet(reg, "tag"), gTag))

		if sEndpoint == "" {
			fmt.Fprintf(c.ErrOut, "warn: service '%s' has build:false but no registry.endpoint configured — skipping image swap (define registry.endpoint, set image:, or set build:true)\n", svc)
			continue
		}

		// Cache mode. Manifest swap rewrites to lok8s.cache, and we queue the
		// remote ref for `lo image cache` to pre-populate.
		remoteRef := sEndpoint + "/" + sBranch + "/" + svc + ":" + sTag
		fmt.Fprintf(queue, "%s\t%s\t%s\t%s\n", svc, remoteRef, sBranch, sTag)

		fmt.Fprintf(&out, "  - name: %s/%s\n    newName: %s/%s/%s\n    newTag: \"%s\"\n", gPrefix, svc, gCache, sBranch, svc, sTag)
	}
	// Command substitution stripped the trailing newline in bash.
	return strings.TrimRight(out.String(), "\n"), nil
}

func subst(s string) string {
	return string(BareEnvsubst([]byte(s)))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
