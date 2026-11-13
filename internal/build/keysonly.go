package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/toolchain"
	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

// KeysOnlyBlankExpr is the yq program that `lo build --keys-only` runs on
// the final stream, after the transformations and envsubst. It sets every
// scalar under data and stringData to "" (the key names stay) in each
// mapping whose kind is Secret, at any depth of each document (a Secret
// document, a List item, a Secret that another object holds), and in each
// item of a SecretList (an item can omit its kind). It touches only the
// data and stringData keys that exist, so a reference such as
// `{kind: Secret, name: tls}` stays as it is. explode(.) first resolves
// the aliases and merge keys, so no value comes back through an alias.
// The bash tree runs the same program (build::keys_only);
// TestKeysOnlyBlankExprMatchesBash checks the text.
const KeysOnlyBlankExpr = `explode(.) | (.. | select(kind == "map") | select(.kind == "Secret")) |= (((.[] | select(key == "data" or key == "stringData")) | .. | select(kind == "scalar" and tag != "!!null")) = "") | (.. | select(kind == "map") | select(.kind == "SecretList") | .[] | select(key == "items") | select(kind == "seq") | .[] | select(kind == "map")) |= (((.[] | select(key == "data" or key == "stringData")) | .. | select(kind == "scalar" and tag != "!!null")) = "")`

// keysOnlyCapability is the line that `Secret --capabilities` prints when
// the plugin has the keys-only run.
const keysOnlyCapability = "keys-only"

// errNoYAML is a stream that does not parse as YAML.
var errNoYAML = errors.New("the stream does not parse as YAML")

// HasKeysOnlyMarker reports whether a rendered stream holds an object with
// the keys-only marker: a mapping, at any depth of any document (a List
// item too), whose metadata.annotations has the key
// secrets.lok8s.dev/keys-only. It parses the stream, so a quoted, escaped
// or flow-style key is the same key. Aliases resolve and merge keys (<<)
// count, as yq reads the stream after explode(.) (bash:
// keysonly::has_marker). It parses every document before it looks at one:
// a stream that does not parse is an error (errNoYAML), and the callers
// refuse it.
func HasKeysOnlyMarker(data []byte) (bool, error) {
	var docs []*yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		doc := &yaml.Node{}
		err := dec.Decode(doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, fmt.Errorf("%w: %v", errNoYAML, err)
		}
		docs = append(docs, doc)
	}
	for _, doc := range docs {
		if markerIn(doc, map[*yaml.Node]bool{}) {
			return true, nil
		}
	}
	return false, nil
}

// markerIn walks every node under n (aliases resolved, each node once)
// and reports whether a mapping has the marker in metadata.annotations.
func markerIn(n *yaml.Node, seen map[*yaml.Node]bool) bool {
	n = resolveAlias(n)
	if n == nil || seen[n] {
		return false
	}
	seen[n] = true
	if n.Kind == yaml.MappingNode {
		for _, md := range mapValues(n, "metadata") {
			for _, ann := range mapValues(md, "annotations") {
				if len(mapValues(ann, secret.KeysOnlyAnnotation)) > 0 {
					return true
				}
			}
		}
	}
	for _, c := range n.Content {
		if markerIn(c, seen) {
			return true
		}
	}
	return false
}

// mapValues returns each value of key in the mapping m: the key itself
// (every occurrence) and the key in each mapping that a merge key (<<)
// brings in. A node that is not a mapping has no values.
func mapValues(m *yaml.Node, key string) []*yaml.Node {
	var out []*yaml.Node
	collectValues(resolveAlias(m), key, &out, map[*yaml.Node]bool{})
	return out
}

func collectValues(m *yaml.Node, key string, out *[]*yaml.Node, seen map[*yaml.Node]bool) {
	if m == nil || m.Kind != yaml.MappingNode || seen[m] {
		return
	}
	seen[m] = true
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := resolveAlias(m.Content[i]), resolveAlias(m.Content[i+1])
		if k == nil || v == nil || k.Kind != yaml.ScalarNode {
			continue
		}
		if k.ShortTag() == "!!merge" {
			if v.Kind == yaml.SequenceNode {
				for _, src := range v.Content {
					collectValues(resolveAlias(src), key, out, seen)
				}
			} else {
				collectValues(v, key, out, seen)
			}
			continue
		}
		if k.Value == key {
			*out = append(*out, v)
		}
	}
}

// resolveAlias follows an alias to the node it names (nil for a chain of
// aliases that does not end).
func resolveAlias(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode; i++ {
		if i == 64 {
			return nil
		}
		n = n.Alias
	}
	return n
}

// keysOnlyRefusal is the output guard's message: an object with the
// keys-only marker never goes into a domain's artifacts (artifacts.yaml
// or the split dir). Keys-only output goes to stdout only.
func keysOnlyRefusal(stderr io.Writer, domainName, artifactPath string) {
	ui.ErrorTo(stderr, "refusing to write %s's render to %s: it holds a keys-only Secret (%s); keys-only output goes to stdout only (lo build --keys-only)",
		domainName, artifactPath, secret.KeysOnlyAnnotation)
}

// keysOnlyUnparsed is the output guard's message for a render that does
// not parse as YAML: the guard cannot look for the marker in it.
func keysOnlyUnparsed(stderr io.Writer, domainName, artifactPath string) {
	ui.ErrorTo(stderr, "refusing to write %s's render to %s: it does not parse as YAML, so it cannot be checked for a keys-only Secret (%s)",
		domainName, artifactPath, secret.KeysOnlyAnnotation)
}

// checkKeysOnlyPlugin refuses a Secret plugin that does not have the
// keys-only run (bash: build::_keys_only_capable). The in-process render
// serves the plugin from this binary, so only the exec pipeline needs the
// check. A plugin older than the keys-only run does not know the setting,
// and a plugin older than v0.2.0 does not know the store-free switch
// either: it would read the store and run the generators. The probe is
// `Secret --capabilities`, run so that no plugin version can do anything
// with it: in an empty temporary directory, with an empty stdin,
// PATH_SECRETS at a path in that directory that does not exist, the
// store-free switch on and the keys-only setting off. A plugin that knows
// the flag prints its capabilities. A plugin from v0.2.0 on that does not
// know the flag stops on the store-free switch and prints nothing. A
// plugin before v0.2.0 opens "--capabilities" as its spec file, which does
// not exist, and fails. hack/proof-secrets-keys-only-skew.sh runs the
// probe on every release. A project without the plugin file renders no
// secrets.lok8s.dev Secret (the render fails on such a generator), so
// there is nothing to check.
func checkKeysOnlyPlugin(ctx context.Context, o *Options, stderr io.Writer) error {
	if mode, err := render.CurrentMode(); err == nil && mode == render.ModeInProcess {
		return nil
	}
	plugin := filepath.Join(config.KustomizePluginHome(o.Paths), filepath.FromSlash(toolchain.SecretPluginRel))
	if !fsutil.FileExists(plugin) {
		return nil
	}
	refuse := func() error {
		ui.ErrorTo(stderr, "the Secret plugin %s does not have the keys-only run (an older plugin would read the secrets store) — run 'lo toolchain install' to install the plugin of this lo",
			config.RelTo(o.Paths.Base, plugin))
		return ErrHandled
	}
	dir, err := os.MkdirTemp("", "lok8s-keys-only-")
	if err != nil {
		return refuse()
	}
	defer func() { _ = os.RemoveAll(dir) }()
	var out strings.Builder
	err = o.runner().Run(ctx, execx.Cmd{
		Name: plugin,
		Args: []string{"--capabilities"},
		Dir:  dir,
		Env: []string{
			"PATH_SECRETS=" + filepath.Join(dir, "store"),
			secret.DisableEnv + "=1",
			secret.KeysOnlyEnv + "=0",
		},
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: io.Discard,
	})
	if err == nil {
		for line := range strings.SplitSeq(out.String(), "\n") {
			if line == keysOnlyCapability {
				return nil
			}
		}
	}
	return refuse()
}

// KeysOnly renders the domain kustomization with the Secret plugin's
// keys-only run and writes the stream to out (bash: build::keys_only). The
// contract: it prints no Secret value. The plugin emits each
// secrets.lok8s.dev Secret with its key names, empty values and the
// keys-only marker annotation. Then KeysOnlyBlankExpr empties every value
// of every Secret in the final stream (after the transformations and
// envsubst): a patch, a plain Secret manifest, a kustomize
// secretGenerator or a Helm chart's Secret prints its key names only. It
// is store-free like --no-secrets (the plugin's store-free switch is on,
// so no store is read, decrypted or minted) and writes no file: no
// artifacts.yaml, no split, no prune of the artifacts dir. In the exec
// pipeline the Secret plugin must have the keys-only run
// (checkKeysOnlyPlugin). The other generators and resources render as in
// a normal build; yq writes the stream, so its layout can differ from
// artifacts.yaml.
func KeysOnly(ctx context.Context, o Options, out io.Writer) error {
	stderr := o.stderr()
	domainDir := filepath.Join(o.Paths.Clusters, o.Domain)
	if !hasKustomization(domainDir) {
		ui.ErrorTo(stderr, "domain %s has no kustomization.yaml — compose its targets there, e.g. resources: [./targets/foo, ../../.targets/bar]", o.Domain)
		return ErrHandled
	}
	if err := checkKeysOnlyPlugin(ctx, &o, stderr); err != nil {
		return err
	}
	yq, ok := execx.Look(o.Paths, "yq")
	if !ok {
		ui.ErrorTo(stderr, "lo build --keys-only: yq not found — install the pinned toolchain (b install)")
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
	var blanked bytes.Buffer
	if err := o.runner().Run(ctx, execx.Cmd{
		Name:   yq,
		Args:   []string{"--yaml-fix-merge-anchor-to-spec", KeysOnlyBlankExpr, "-"},
		Stdin:  bytes.NewReader(rendered),
		Stdout: &blanked,
		Stderr: stderr,
	}); err != nil {
		ui.ErrorTo(stderr, "lo build --keys-only: yq could not empty the Secret values of %s's render", o.Domain)
		return ErrHandled
	}
	docs := countKindLines(blanked.Bytes())
	if docs == 0 {
		ui.WarnTo(stderr, "%s rendered 0 documents — is its kustomization.yaml composing any targets?", o.Domain)
	}
	if out == nil {
		out = os.Stdout
	}
	if _, err := out.Write(blanked.Bytes()); err != nil {
		ui.ErrorTo(stderr, "lo build --keys-only: writing %s's render to stdout failed", o.Domain)
		return ErrHandled
	}
	fmt.Fprintf(stderr, "lo build: %s rendered %d document(s) -> stdout (keys only)\n", o.Domain, docs)
	return nil
}
