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
// the final stream, after the transformations and envsubst. It changes
// each Secret: a mapping whose kind is Secret, at any depth of each
// document (a Secret document, a List item, a Secret that another object
// holds), that has metadata, data or stringData, and each item of a
// SecretList (an item can omit its kind). In each Secret it:
//
//   - sets every scalar under data and stringData to "" (the key names
//     stay),
//   - sets every value of metadata.labels and metadata.annotations to ""
//     (the keys stay),
//   - sets the keys-only marker (secrets.lok8s.dev/keys-only: "true"), so
//     that the build guard and lo deploy refuse the output, and
//   - removes every comment of the Secret.
//
// It touches only the keys that exist, except the marker, so a reference
// such as `{kind: Secret, name: tls}` (no metadata, no data) stays as it
// is. explode(.) first resolves the aliases and merge keys, so no value
// comes back through an alias. Objects that are not Secrets print as they
// are rendered. The bash tree runs the same program (build::keys_only);
// TestKeysOnlyBlankExprMatchesBash checks the text.
const KeysOnlyBlankExpr = `explode(.) | (.. | select(kind == "map") | select(.kind == "Secret") | select(has("metadata") or has("data") or has("stringData"))) |= (((.[] | select(key == "data" or key == "stringData")) | .. | select(kind == "scalar" and tag != "!!null")) = "" | ((.[] | select(key == "metadata") | select(kind == "map") | .[] | select(key == "labels" or key == "annotations") | select(kind == "map")) | .. | select(kind == "scalar" and tag != "!!null")) = "" | .metadata.annotations["secrets.lok8s.dev/keys-only"] = "true" | ... comments = "") | (.. | select(kind == "map") | select(.kind == "SecretList") | .[] | select(key == "items") | select(kind == "seq") | .[] | select(kind == "map")) |= (((.[] | select(key == "data" or key == "stringData")) | .. | select(kind == "scalar" and tag != "!!null")) = "" | ((.[] | select(key == "metadata") | select(kind == "map") | .[] | select(key == "labels" or key == "annotations") | select(kind == "map")) | .. | select(kind == "scalar" and tag != "!!null")) = "" | .metadata.annotations["secrets.lok8s.dev/keys-only"] = "true" | ... comments = "")`

// lastAppliedAnnotation is the annotation that `kubectl apply` writes. It
// holds the object as it was applied, so on a Secret it can hold the
// Secret's values.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// KeysOnlyLastAppliedExpr is the yq program (run with -N) that `lo build
// --keys-only` runs on the emptied stream. It prints one line,
// "Secret <namespace>/<name>", for each Secret that has the annotation
// kubectl.kubernetes.io/last-applied-configuration in metadata.annotations:
// a mapping whose kind is Secret at any depth, or an item of a SecretList.
// It prints nothing for a stream without such a Secret and fails on a
// stream that does not parse. A namespace or a name that is not a scalar
// is left out of the line. KeysOnlyBlankExpr empties the value of the
// annotation but keeps its key, so this program still finds it. KeysOnly
// refuses the render when there is a line, and does not only empty it:
// the source of such a Secret holds its values, and the output must not
// look complete when it is not. The bash tree runs the same program
// (build::keys_only);
// TestKeysOnlyLastAppliedExprMatchesBash checks the text.
const KeysOnlyLastAppliedExpr = `explode(.) | ((.. | select(kind == "map") | select(.kind == "Secret")), (.. | select(kind == "map") | select(.kind == "SecretList") | .[] | select(key == "items") | select(kind == "seq") | .[] | select(kind == "map") | select(.kind != "Secret"))) | select(.metadata | kind == "map") | select(.metadata.annotations | kind == "map") | select(.metadata.annotations | has("kubectl.kubernetes.io/last-applied-configuration")) | .metadata | pick(["namespace", "name"]) | with_entries(select((.value | kind) == "scalar" and (.value | tag) != "!!null")) | to_entries | map(.value) | join("/") | sub("^", "Secret ")`

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
			return false, fmt.Errorf("%w: %w", errNoYAML, err)
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
// probe on every release.
//
// The plugin home must exist. kustomize takes the first plugin home that
// exists: KUSTOMIZE_PLUGIN_HOME, then directories under XDG_CONFIG_HOME
// and HOME. With an absent home it would load a plugin that lo did not
// check. The home that lo checked goes to kustomize as
// KUSTOMIZE_PLUGIN_HOME (o.pluginHome). A home without the plugin file
// renders no secrets.lok8s.dev Secret (the render fails on such a
// generator), so there is no plugin to check.
//
// The probe is a compatibility check, not isolation or attestation: a
// plugin that lies about its capabilities, or a file that is changed
// after the probe, gets through.
func checkKeysOnlyPlugin(ctx context.Context, o *Options, stderr io.Writer) error {
	if mode, err := render.CurrentMode(); err == nil && mode == render.ModeInProcess {
		return nil
	}
	home := config.KustomizePluginHome(o.Paths)
	if !fsutil.DirExists(home) {
		ui.ErrorTo(stderr, "lo build --keys-only: the kustomize plugin home %s does not exist, so kustomize would load plugins from other directories that lo does not check — run 'lo toolchain install' to install the plugin of this lo",
			config.RelTo(o.Paths.Base, home))
		return ErrHandled
	}
	o.pluginHome = home
	plugin := filepath.Join(home, filepath.FromSlash(toolchain.SecretPluginRel))
	if !fsutil.FileExists(plugin) {
		return nil
	}
	rel := config.RelTo(o.Paths.Base, plugin)
	refuse := func() error {
		ui.ErrorTo(stderr, "the Secret plugin %s does not have the keys-only run (an older plugin would read the secrets store) — run 'lo toolchain install' to install the plugin of this lo", rel)
		return ErrHandled
	}
	dir, err := os.MkdirTemp("", "lok8s-keys-only-")
	if err != nil {
		ui.ErrorTo(stderr, "lo build --keys-only: cannot make a temporary directory for the check of the Secret plugin %s: %v", rel, err)
		return ErrHandled
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
	if ctx.Err() != nil || execx.ExitCode(err) > 128 {
		ui.ErrorTo(stderr, "lo build --keys-only: the check of the Secret plugin %s was interrupted", rel)
		return ErrHandled
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
// secretGenerator or a Helm chart's Secret prints its key names only. A
// Secret with the annotation kubectl.kubernetes.io/last-applied-configuration
// is refused (KeysOnlyLastAppliedExpr): the annotation can hold the values,
// and nothing goes to stdout. It is store-free like --no-secrets (the plugin's store-free switch is on,
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
	var lastApplied bytes.Buffer
	if err := o.runner().Run(ctx, execx.Cmd{
		Name:   yq,
		Args:   []string{"-N", "--yaml-fix-merge-anchor-to-spec", KeysOnlyLastAppliedExpr, "-"},
		Stdin:  bytes.NewReader(blanked.Bytes()),
		Stdout: &lastApplied,
		Stderr: stderr,
	}); err != nil {
		ui.ErrorTo(stderr, "lo build --keys-only: yq could not check %s's render for the annotation %s", o.Domain, lastAppliedAnnotation)
		return ErrHandled
	}
	if found := strings.TrimRight(lastApplied.String(), "\n"); found != "" {
		for line := range strings.SplitSeq(found, "\n") {
			ui.ErrorTo(stderr, "lo build --keys-only: refusing %s's render: %s has the annotation %s, which can hold the Secret's values",
				o.Domain, line, lastAppliedAnnotation)
		}
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
