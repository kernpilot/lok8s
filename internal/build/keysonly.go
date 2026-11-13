package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

// keysOnlyMarker matches the line of the keys-only marker annotation that
// the Secret plugin's keys-only run puts on each Secret it emits (an
// indented `secrets.lok8s.dev/keys-only:` key). A line rule, like the
// document count (countKindLines), so the two implementations agree on
// any input (bash: grep -E '^[[:blank:]]+secrets\.lok8s\.dev/keys-only:').
var keysOnlyMarker = regexp.MustCompile(`(?m)^[ \t]+` + regexp.QuoteMeta(secret.KeysOnlyAnnotation) + `:`)

// hasKeysOnlyMarker reports whether a rendered stream holds an object
// with the keys-only marker.
func hasKeysOnlyMarker(data []byte) bool { return keysOnlyMarker.Match(data) }

// keysOnlyRefusal is the output guard's message: an object with the
// keys-only marker never goes into a domain's artifacts (artifacts.yaml
// or the split dir). Keys-only output goes to stdout only.
func keysOnlyRefusal(stderr io.Writer, domainName, artifactPath string) {
	ui.ErrorTo(stderr, "refusing to write %s's render to %s: it holds a keys-only Secret (%s); keys-only output goes to stdout only (lo build --keys-only)",
		domainName, artifactPath, secret.KeysOnlyAnnotation)
}

// KeysOnly renders the domain kustomization with the Secret plugin's
// keys-only run and writes the stream to out (bash: build::keys_only):
// each secrets.lok8s.dev Secret with its key names and empty values, plus
// the keys-only marker annotation. It is store-free like --no-secrets (the
// plugin's store-free switch is on, so no store is read, decrypted or
// minted) and writes no file: no artifacts.yaml, no split, no prune of the
// artifacts dir. The other generators and resources render as in a
// normal build.
func KeysOnly(ctx context.Context, o Options, out io.Writer) error {
	stderr := o.stderr()
	domainDir := filepath.Join(o.Paths.Clusters, o.Domain)
	if !hasKustomization(domainDir) {
		ui.ErrorTo(stderr, "domain %s has no kustomization.yaml — compose its targets there, e.g. resources: [./targets/foo, ../../.targets/bar]", o.Domain)
		return ErrHandled
	}
	exportSecretsPath(domainDir)
	resolveAPI(o.Paths, domainDir)
	kubeconfig := renderKubeconfig(o.Paths, o.Domain)
	whitelist := EnvsubstWhitelist()

	o.NoSecrets = true
	o.KeysOnly = true
	rendered, err := runKustomize(ctx, o, domainDir, kubeconfig, stderr)
	if err != nil {
		ui.ErrorTo(stderr, "kustomize build failed for %s", o.Domain)
		return ErrHandled
	}
	rendered = Envsubst(rendered, whitelist)
	docs := countKindLines(rendered)
	if docs == 0 {
		ui.WarnTo(stderr, "%s rendered 0 documents — is its kustomization.yaml composing any targets?", o.Domain)
	}
	if out == nil {
		out = os.Stdout
	}
	if _, err := out.Write(rendered); err != nil {
		ui.ErrorTo(stderr, "kustomize build failed for %s", o.Domain)
		return ErrHandled
	}
	fmt.Fprintf(stderr, "lo build: %s rendered %d document(s) -> stdout (keys only)\n", o.Domain, docs)
	return nil
}
