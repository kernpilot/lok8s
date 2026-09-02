// Package hooks is the Go port of .lok8s/libs/hooks — dev-time lifecycle
// actions on rendered artifacts, selected by label.
//
// INTERNAL: driven by the Tilt `hooks:` wrapper (a local_resource cmd), not a
// user workflow. Acts on the live cluster + the domain's rendered artifacts
// to re-run/refresh a LABELLED subset when a watched file changes:
//
//	recreate — delete + apply the matched objects (immutable Jobs re-run)
//	restart  — rollout restart the matched workloads
//	apply    — re-apply the matched objects (no delete)
//
// Targeting is by LABEL — the same labels lok8s applies — passed
// kubectl-style: lo hooks recreate --selector lok8s.dev/type=seed,…
// The selector validation and the label filter are native (the bash built a
// yq `select(...)` expression; the ACCEPTED grammar and error strings are
// identical); kubectl and the tilt probes run through the Runner seam.
package hooks

import (
	"context"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kapply"
	"github.com/kernpilot/lok8s/internal/tilt"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrHandled marks a failure whose message was already printed in the bash
// [error] format; callers exit non-zero without printing anything further.
var ErrHandled = errors.New("hooks: handled")

// Context carries one hooks-command invocation. LOK8S_NONINTERACTIVE=1 is
// exported by the command layer before anything runs (bash: main::hooks) —
// Tilt's local_resource has no controlling terminal, so kapply must never
// try the /dev/tty progress UI or confirm prompts.
type Context struct {
	Paths  *config.Paths
	Runner execx.Runner
	Out    io.Writer
	ErrOut io.Writer
	// Domain is the parent's resolved global arg (bash: consumed via
	// :^~domain), so the Tilt wrapper never has to pass it.
	Domain string

	// Tilt answers the session probes (Running/HasResource/
	// TriggerResource). nil → a Context over the same Runner.
	Tilt *tilt.Context
	// Applier runs the kapply::apply port. nil → kapply.NewApplier.
	Applier *kapply.Applier
}

func (c *Context) tiltCtx() *tilt.Context {
	if c.Tilt != nil {
		return c.Tilt
	}
	return &tilt.Context{Paths: c.Paths, Runner: c.Runner, Out: c.Out, ErrOut: c.ErrOut}
}

func (c *Context) applier() *kapply.Applier {
	if c.Applier != nil {
		return c.Applier
	}
	return kapply.NewApplier(c.Runner, c.Out, c.ErrOut)
}

// selClauseRe is the label-safe allowlist for selector keys AND values —
// the same set the Starlark _SEL_CHARS guard bans everything outside of.
var selClauseRe = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)

// selectorPair is one parsed k=v clause.
type selectorPair struct{ key, val string }

// parseSelector validates a kubectl-style label selector (k=v,k2=v2) with
// the exact grammar and error strings of hooks::_yq_filter (which built a yq
// `select(...)` from it — the validation existed to prevent yq injection;
// the native filter keeps it as the shared contract).
func (c *Context) parseSelector(selector string) ([]selectorPair, error) {
	if selector == "" {
		ui.Errorf(c.ErrOut, "hooks: --selector is required")
		return nil, ErrHandled
	}
	var pairs []selectorPair
	for _, clause := range strings.Split(selector, ",") {
		if !strings.Contains(clause, "=") {
			ui.Errorf(c.ErrOut, "hooks: selector clause '%s' must be key=value", clause)
			return nil, ErrHandled
		}
		key, val, _ := strings.Cut(clause, "=")
		if !selClauseRe.MatchString(key) || !selClauseRe.MatchString(val) {
			ui.Errorf(c.ErrOut, "hooks: invalid selector clause '%s' (key/value must be [a-zA-Z0-9._/-])", clause)
			return nil, ErrHandled
		}
		pairs = append(pairs, selectorPair{key, val})
	}
	if len(pairs) == 0 {
		ui.Errorf(c.ErrOut, "hooks: empty --selector")
		return nil, ErrHandled
	}
	return pairs, nil
}

// doc is one parsed document of the rendered artifact.
type doc struct {
	node *yaml.Node
}

func (d doc) scalar(path ...string) string {
	cur := deref(d.node)
	for _, key := range path {
		cur = mapGet(cur, key)
		if cur == nil {
			return ""
		}
	}
	if cur.Kind != yaml.ScalarNode {
		return ""
	}
	return cur.Value
}

func parseDocs(data []byte) []doc {
	var docs []doc
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	for {
		var n yaml.Node
		if err := dec.Decode(&n); err != nil {
			break
		}
		docs = append(docs, doc{node: &n})
	}
	return docs
}

// matches applies the label clauses with yq's `==` semantics: the label must
// be a string scalar equal to the value (an unquoted `count: 5` int label
// never equals the string "5", exactly as in yq).
func (d doc) matches(pairs []selectorPair) bool {
	labels := mapGetPath(d.node, "metadata", "labels")
	for _, p := range pairs {
		v := mapGet(labels, p.key)
		if v == nil || v.Kind != yaml.ScalarNode || v.Tag != "!!str" || v.Value != p.val {
			return false
		}
	}
	return true
}

// selectObjects emits the objects matching the label selector from the
// domain's rendered artifact (bash: hooks::_select). This is the same single
// clusters/<domain>/artifacts.yaml `lo deploy` applies; the Tilt reload
// keeps the upstream ConfigMaps current, so a recreated Job mounts the
// edited content. A missing artifact selects nothing; unreadable YAML too
// (bash: `yq … 2>/dev/null || true`).
func (c *Context) selectObjects(domainName, selector string) ([]doc, error) {
	pairs, err := c.parseSelector(selector)
	if err != nil {
		return nil, err
	}
	raw, err := readFile(c.Paths.Clusters + "/" + domainName + "/artifacts.yaml")
	if err != nil {
		return nil, nil
	}
	var out []doc
	for _, d := range parseDocs(raw) {
		if d.matches(pairs) {
			out = append(out, d)
		}
	}
	return out, nil
}

// marshalDocs renders documents back to a `---`-separated stream (feeds
// kubectl delete/apply; the exact byte shape is yq-adjacent, the semantics
// identical).
func marshalDocs(docs []doc) string {
	var buf strings.Builder
	for i, d := range docs {
		if i > 0 {
			buf.WriteString("---\n")
		}
		var one strings.Builder
		enc := yaml.NewEncoder(&one)
		enc.SetIndent(2)
		if enc.Encode(d.node) == nil {
			_ = enc.Close()
			buf.WriteString(one.String())
		}
	}
	return buf.String()
}

// hasObjects mirrors deploy::_has_objects: a stream counts as non-empty when
// any line declares a kind.
var kindLineRe = regexp.MustCompile(`(?m)^kind:[ \t]+[A-Za-z]`)

// objectNames is hooks::_object_names — the metadata.name of every selected
// object. (The bash filtered yq's `---` separators out of the list; the
// native walk never produces them.)
func objectNames(docs []doc) []string {
	var names []string
	for _, d := range docs {
		if n := d.scalar("metadata", "name"); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// liveImage is one running container's {name, image}.
type liveImage struct{ name, image string }

// liveImages reads the running object's container images (bash:
// hooks::_live_images). Empty when the object does not exist yet (a
// first-ever apply has nothing to preserve).
func (c *Context) liveImages(ctx context.Context, kind, name, ns string) []liveImage {
	if kind == "" || name == "" {
		return nil
	}
	args := []string{"get", kind, name}
	if ns != "" {
		args = append(args, "-n", ns)
	}
	args = append(args, "-o", "json")
	var buf strings.Builder
	if err := c.Runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: args,
		Stdout: &buf, Stderr: io.Discard}); err != nil {
		return nil
	}
	if strings.TrimSpace(buf.String()) == "" {
		return nil
	}
	// YAML is a JSON superset — the same node walk reads the live object.
	var n yaml.Node
	if yaml.Unmarshal([]byte(buf.String()), &n) != nil {
		return nil
	}
	var out []liveImage
	for _, cn := range containerNodes(&n) {
		out = append(out, liveImage{
			name:  scalarOf(mapGet(cn, "name")),
			image: scalarOf(mapGet(cn, "image")),
		})
	}
	return out
}

// containerNodes yields .spec.template.spec.containers[] +
// .spec.template.spec.initContainers[].
func containerNodes(root *yaml.Node) []*yaml.Node {
	spec := mapGetPath(root, "spec", "template", "spec")
	var out []*yaml.Node
	for _, key := range []string{"containers", "initContainers"} {
		list := mapGet(spec, key)
		if list == nil || list.Kind != yaml.SequenceNode {
			continue
		}
		for _, item := range list.Content {
			out = append(out, deref(item))
		}
	}
	return out
}

// overlayImages rewrites one rendered object with each container's image
// replaced by the ref the CLUSTER is running for the container of the same
// name (bash: hooks::_overlay_images). Containers absent from the live
// object keep their rendered ref.
//
// WHY THIS EXISTS: `recreate` re-applies from the rendered artifact, which
// carries the DECLARED ref (e.g. `lok8s.local/<name>`). In a Tilt dev loop
// the running object carries Tilt's built-and-pushed ref — image injection
// happens on Tilt's deploy path, not in the YAML. Re-applying the declared
// ref points the workload at an image nobody pushed, and it sits in
// ImagePullBackOff forever while Tilt still reports the resource ok —
// measured on a migrate Job whose DB migration sat blocked exactly this way.
func overlayImages(d doc, live []liveImage) {
	for _, cn := range containerNodes(d.node) {
		cname := scalarOf(mapGet(cn, "name"))
		for _, l := range live {
			if l.name == cname {
				if img := mapGet(cn, "image"); img != nil {
					img.Value = l.image
					img.Tag = "!!str"
					img.Style = 0
				}
				break
			}
		}
	}
}

// preserveImages is hooks::_preserve_images: each selected object with live
// image refs overlaid where the object is still running.
func (c *Context) preserveImages(ctx context.Context, docs []doc) {
	for _, d := range docs {
		kind := d.scalar("kind")
		name := d.scalar("metadata", "name")
		ns := d.scalar("metadata", "namespace")
		imgs := c.liveImages(ctx, kind, name, ns)
		if len(imgs) > 0 {
			overlayImages(d, imgs)
		}
	}
}

// tiltCanRecreate reports whether the running Tilt session owns EVERY
// selected object (bash: hooks::_tilt_can_recreate). All-or-nothing on
// purpose: recreating half a set through Tilt and half through kubectl
// would leave the two halves on different image refs, which is a subtler
// version of the bug this exists to avoid.
func (c *Context) tiltCanRecreate(ctx context.Context, docs []doc, port string) bool {
	names := objectNames(docs)
	for _, name := range names {
		if !c.tiltCtx().HasResource(ctx, name, port) {
			return false
		}
	}
	return len(names) > 0
}

// Recreate deletes + applies the selected objects so immutable Jobs re-run
// (bash: hooks::recreate).
func (c *Context) Recreate(ctx context.Context, selector string) error {
	docs, err := c.selectObjects(c.Domain, selector)
	if err != nil {
		return err
	}
	objs := marshalDocs(docs)
	if !kindLineRe.MatchString(objs) {
		ui.Warnf(c.ErrOut, "hooks recreate: no objects match '%s'", selector)
		return nil
	}
	// UNDER TILT, LET TILT DO IT. The rendered artifact carries the DECLARED
	// image ref while the built-and-pushed image only exists under Tilt's
	// content-hash tag (full rationale at overlayImages). Preserving the
	// LIVE object's refs fixes that only while something is running: a Job
	// that has completed and been cleaned up has nothing to preserve, and
	// the raw ref ships anyway — measured, a migration Job broke again
	// exactly this way after the preservation fix landed. Tilt is the only
	// party that knows the real ref in both cases, so when a Tilt session
	// owns this resource, hand the work to it.
	tc := c.tiltCtx()
	port := tc.Port()
	if tc.Running(ctx, port) && c.tiltCanRecreate(ctx, docs, port) {
		for _, name := range objectNames(docs) {
			if err := tc.TriggerResource(ctx, name, port); err != nil {
				ui.Errorf(c.ErrOut, "hooks recreate: tilt trigger failed for '%s'", name)
				return ErrHandled
			}
			ui.Debugf(c.ErrOut, "hooks: recreated '%s' through Tilt (image injection preserved)", name)
		}
		return nil
	}

	// No Tilt session (or it does not own these objects): fall back to
	// delete + apply, carrying over the live image refs where an object is
	// still running. Atomic-ish: delete (tolerating not-found) THEN apply —
	// chained so a real delete error stops before apply, and an apply
	// failure surfaces, making a half-done recreate visible rather than
	// silently destructive. True one-shot atomicity isn't possible for an
	// immutable Job.
	c.preserveImages(ctx, docs)
	applied := marshalDocs(docs)
	if err := c.Runner.Run(ctx, execx.Cmd{
		Name: "kubectl", Args: []string{"delete", "--ignore-not-found", "-f", "-"},
		Stdin: strings.NewReader(objs + "\n"), Stdout: c.Out, Stderr: c.ErrOut,
	}); err != nil {
		return ErrHandled
	}
	if _, rc := c.applier().Apply(ctx, "hook recreate "+selector, applied+"\n"); rc != 0 {
		return ErrHandled
	}
	ui.Debugf(c.ErrOut, "hooks: recreated objects matching '%s'", selector)
	return nil
}

// Apply re-applies the selected objects, no delete (bash: hooks::apply).
func (c *Context) Apply(ctx context.Context, selector string) error {
	docs, err := c.selectObjects(c.Domain, selector)
	if err != nil {
		return err
	}
	objs := marshalDocs(docs)
	if !kindLineRe.MatchString(objs) {
		ui.Warnf(c.ErrOut, "hooks apply: no objects match '%s'", selector)
		return nil
	}
	if _, rc := c.applier().Apply(ctx, "hook apply "+selector, objs+"\n"); rc != 0 {
		return ErrHandled
	}
	ui.Debugf(c.ErrOut, "hooks: applied objects matching '%s'", selector)
	return nil
}

// Restart rollout-restarts the selected workloads (bash: hooks::restart).
func (c *Context) Restart(ctx context.Context, selector string) error {
	docs, err := c.selectObjects(c.Domain, selector)
	if err != nil {
		return err
	}
	objs := marshalDocs(docs)
	if !kindLineRe.MatchString(objs) {
		ui.Warnf(c.ErrOut, "hooks restart: no workloads match '%s'", selector)
		return nil
	}
	// Rollout-restart each matched Deployment/StatefulSet/DaemonSet by
	// kind/name+ns.
	n := 0
	for _, d := range docs {
		kind := d.scalar("kind")
		if kind != "Deployment" && kind != "StatefulSet" && kind != "DaemonSet" {
			continue
		}
		name := d.scalar("metadata", "name")
		if name == "" {
			continue
		}
		ns := d.scalar("metadata", "namespace")
		if ns == "" {
			ns = "default"
		}
		ui.Debugf(c.ErrOut, "rollout restart %s/%s -n %s", kind, name, ns)
		err := c.Runner.Run(ctx, execx.Cmd{
			Name: "kubectl", Args: []string{"-n", ns, "rollout", "restart", strings.ToLower(kind) + "/" + name},
			Stdout: c.Out, Stderr: io.Discard,
		})
		if err == nil {
			n++
		} else {
			ui.Warnf(c.ErrOut, "hooks restart: %s/%s (-n %s) failed", kind, name, ns)
		}
	}
	if n > 0 {
		ui.Debugf(c.ErrOut, "hooks: restarted %d workload(s) matching '%s'", n, selector)
	} else {
		ui.Warnf(c.ErrOut, "hooks restart: matched objects but none are restartable (Deployment/StatefulSet/DaemonSet) for '%s'", selector)
	}
	return nil
}

// ── yaml.Node helpers ────────────────────────────────────

func deref(n *yaml.Node) *yaml.Node {
	for n != nil && (n.Kind == yaml.DocumentNode || n.Kind == yaml.AliasNode) {
		if n.Kind == yaml.DocumentNode {
			if len(n.Content) == 0 {
				return nil
			}
			n = n.Content[0]
			continue
		}
		n = n.Alias
	}
	return n
}

func mapGet(n *yaml.Node, key string) *yaml.Node {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return deref(n.Content[i+1])
		}
	}
	return nil
}

func mapGetPath(n *yaml.Node, keys ...string) *yaml.Node {
	cur := deref(n)
	for _, key := range keys {
		cur = mapGet(cur, key)
		if cur == nil {
			return nil
		}
	}
	return cur
}

func scalarOf(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
