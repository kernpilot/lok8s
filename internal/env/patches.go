package env

// patches.go — services.<name>.patches: kustomize patches that a services
// profile declares for one service (bash: env::generate_patches). Each patch
// is checked against the domain artifact, then emitted into the Tilt overlay
// with a label selector that keeps it on that service's objects.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// serviceLabel routes a rendered object to its service (the Tilt
// extension's default label_prefix + "/name").
const serviceLabel = "lok8s.dev/name"

var (
	patchKeys = []string{"patch", "target"}
	// patchTargetKeys is also the emit order of the target fields.
	patchTargetKeys = []string{"group", "version", "kind", "name", "namespace"}
	// targetValueRE admits the characters of a DNS-subdomain name, plus the
	// colon of RBAC names (system:...). The dot is the only regex
	// metacharacter in it, and the emit escapes it: kustomize reads these
	// fields as anchored regular expressions.
	targetValueRE = regexp.MustCompile(`^[A-Za-z0-9.:-]+$`)
	// labelValueRE is the Kubernetes label-value syntax (the selector value).
	labelValueRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)
	// patchControlRE finds a character the block scalar of the emit cannot
	// carry: a C0 or C1 control other than tab and newline, a line or
	// paragraph separator (YAML parsers that read YAML 1.1 break the line
	// there, as at NEL), a byte-order mark, or a noncharacter. Bash runs the
	// same expression through yq's test(), on the same regex engine.
	patchControlRE = regexp.MustCompile(`[\x{00}-\x{08}\x{0B}-\x{1F}\x{7F}-\x{9F}\x{2028}\x{2029}\x{FEFF}\x{FFFE}\x{FFFF}]`)
)

// artifactObject is the part of a rendered object that a patch target is
// matched against.
type artifactObject struct {
	group, version, kind, name, namespace, service string
}

// generatePatches validates every services.<name>.patches entry, matches it
// against the domain artifact and returns the `patches:` list items for the
// overlay ("" when no enabled service declares a patch). A failure prints one
// [error] line and returns ErrHandled.
func (c *Context) generatePatches(doc *yaml.Node, domainArtifact string) (string, error) {
	services := yqsem.Lookup(doc, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return "", nil
	}

	var objects []artifactObject
	loaded := false
	var out strings.Builder
	for i := 0; i+1 < len(services.Content); i += 2 {
		svc := services.Content[i].Value
		if svc == "" {
			continue
		}
		svcNode := yqsem.Deref(services.Content[i+1])

		// A disabled service gets no patches, as it gets no image swap.
		if yqsem.ToString(yqsem.MapGet(svcNode, "enabled")) == "false" {
			continue
		}

		list := yqsem.Deref(yqsem.MapGet(svcNode, "patches"))
		if yqsem.IsNull(list) {
			continue
		}
		if list.Kind != yaml.SequenceNode {
			return "", c.patchError("services.%s.patches must be a list", svc)
		}
		if len(list.Content) == 0 {
			continue
		}
		if !labelValueRE.MatchString(svc) {
			return "", c.patchError("services.%s: a service with patches needs a name that is a valid label value", svc)
		}

		for idx, item := range list.Content {
			where := fmt.Sprintf("services.%s.patches[%d]", svc, idx)
			patch, target, err := c.validatePatch(where, yqsem.Deref(item))
			if err != nil {
				return "", err
			}

			if !loaded {
				if objects, err = readArtifactObjects(domainArtifact); err != nil {
					return "", c.patchError("cannot read the %s artifacts", c.Domain)
				}
				loaded = true
			}
			if !matchesAny(objects, target, svc) {
				return "", c.patchError("%s: target %s/%s matches no object labelled %s=%s in the %s artifacts",
					where, target["kind"], target["name"], serviceLabel, svc, c.Domain)
			}

			writePatch(&out, target, svc, patch)
		}
	}
	// Command substitution stripped the trailing newline in bash.
	return strings.TrimRight(out.String(), "\n"), nil
}

// validatePatch checks one patches entry and returns its patch text (trailing
// newlines removed, as bash's command substitution does) and its target.
func (c *Context) validatePatch(where string, item *yaml.Node) (string, map[string]string, error) {
	if item == nil || item.Kind != yaml.MappingNode {
		return "", nil, c.patchError("%s must be a mapping with 'patch' and 'target'", where)
	}
	for i := 0; i+1 < len(item.Content); i += 2 {
		if k := item.Content[i].Value; !slices.Contains(patchKeys, k) {
			return "", nil, c.patchError("%s.%s unknown (allowed: patch, target)", where, k)
		}
	}

	pn := yqsem.Deref(yqsem.MapGet(item, "patch"))
	if pn == nil || pn.Kind != yaml.ScalarNode || pn.Tag != "!!str" {
		return "", nil, c.patchError("%s.patch must be a non-empty string", where)
	}
	if patchControlRE.MatchString(pn.Value) {
		return "", nil, c.patchError("%s.patch contains a control or line-separator character (only tab and newline are allowed)", where)
	}
	if strings.Trim(pn.Value, " \t\n") == "" {
		return "", nil, c.patchError("%s.patch must be a non-empty string", where)
	}

	tn := yqsem.Deref(yqsem.MapGet(item, "target"))
	if tn == nil || tn.Kind != yaml.MappingNode {
		return "", nil, c.patchError("%s.target must be a mapping", where)
	}
	for i := 0; i+1 < len(tn.Content); i += 2 {
		if k := tn.Content[i].Value; !slices.Contains(patchTargetKeys, k) {
			return "", nil, c.patchError("%s.target.%s unknown (allowed: group, version, kind, name, namespace)", where, k)
		}
	}
	if yqsem.IsNull(yqsem.MapGet(tn, "kind")) || yqsem.IsNull(yqsem.MapGet(tn, "name")) {
		return "", nil, c.patchError("%s.target.kind and %s.target.name are required", where, where)
	}
	target := map[string]string{}
	for _, k := range patchTargetKeys {
		v := yqsem.Deref(yqsem.MapGet(tn, k))
		if v == nil {
			continue
		}
		if v.Kind != yaml.ScalarNode || v.Tag == "!!null" || !targetValueRE.MatchString(v.Value) {
			return "", nil, c.patchError("%s.target.%s must match [A-Za-z0-9.:-]+", where, k)
		}
		target[k] = v.Value
	}
	return strings.TrimRight(pn.Value, "\n"), target, nil
}

// writePatch emits one overlay `patches:` item. The target values are
// single-quoted with every dot escaped, so kustomize's anchored regex match
// is an exact match. The patch is a block scalar with an explicit
// indentation indicator, so a first line that starts with a space keeps it.
func writePatch(out *strings.Builder, target map[string]string, svc, patch string) {
	out.WriteString("  - target:\n")
	for _, k := range patchTargetKeys {
		if v, ok := target[k]; ok {
			fmt.Fprintf(out, "      %s: '%s'\n", k, strings.ReplaceAll(v, ".", `\.`))
		}
	}
	fmt.Fprintf(out, "      labelSelector: '%s=%s'\n", serviceLabel, svc)
	out.WriteString("    patch: |2-\n")
	for line := range strings.SplitSeq(patch, "\n") {
		if line == "" {
			out.WriteString("\n") // an empty line needs no indentation
			continue
		}
		fmt.Fprintf(out, "      %s\n", line)
	}
}

// readArtifactObjects reads the routing fields of every object in the domain
// artifact. A missing artifact holds no objects.
func readArtifactObjects(path string) ([]artifactObject, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var objects []artifactObject
	dec := yaml.NewDecoder(f)
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return objects, nil
			}
			return nil, err
		}
		root := yqsem.Deref(&doc)
		if root == nil || root.Kind != yaml.MappingNode {
			continue
		}
		o := artifactObject{
			kind:      scalarValue(yqsem.MapGet(root, "kind")),
			name:      scalarValue(yqsem.Lookup(root, "metadata", "name")),
			namespace: scalarValue(yqsem.Lookup(root, "metadata", "namespace")),
			service:   scalarValue(yqsem.Lookup(root, "metadata", "labels", serviceLabel)),
		}
		// The group is the part of apiVersion before the first slash, the
		// version the part after the last (bash: split("/")). For a valid
		// apiVersion ("v1" or "<group>/<version>") that is kustomize's split.
		parts := strings.Split(scalarValue(yqsem.MapGet(root, "apiVersion")), "/")
		o.version = parts[len(parts)-1]
		if len(parts) > 1 {
			o.group = parts[0]
		}
		objects = append(objects, o)
	}
}

// matchesAny reports whether one object of the service matches the target.
// An unset target field matches any value.
func matchesAny(objects []artifactObject, target map[string]string, svc string) bool {
	for _, o := range objects {
		if o.service != svc || o.kind != target["kind"] || o.name != target["name"] {
			continue
		}
		// kustomize compares the effective namespace: an object without one
		// is in "default" (kyaml resid.EffectiveNamespace).
		if ns, ok := target["namespace"]; ok && o.namespace != ns && (o.namespace != "" || ns != "default") {
			continue
		}
		if g, ok := target["group"]; ok && o.group != g {
			continue
		}
		if v, ok := target["version"]; ok && o.version != v {
			continue
		}
		return true
	}
	return false
}

func (c *Context) patchError(format string, a ...any) error {
	ui.ErrorTo(c.ErrOut, format, a...)
	return ErrHandled
}

// scalarValue is a scalar's text, "" for a missing, null or non-scalar node.
func scalarValue(n *yaml.Node) string {
	if yqsem.IsNull(n) {
		return ""
	}
	return yqsem.Scalar(n)
}
