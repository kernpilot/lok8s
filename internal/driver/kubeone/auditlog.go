package kubeone

// auditlog.go ports kubeone::_inject_audit_log: spec.auditLog becomes
// KubeOne's features.staticAuditLog, so the apiserver on every control
// plane writes an audit log. KubeOne uploads the policy file to each
// control plane itself, thus the file only has to exist on this machine.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// The audit Policy type the policy file must declare. The apiserver
// serves only this version of the audit API.
const (
	auditPolicyAPIVersion = "audit.k8s.io/v1"
	auditPolicyKind       = "Policy"
)

// auditLogLimits maps each optional spec.auditLog limit to its
// features.staticAuditLog.config field. The order is the order of the
// checks and of the merge (bash: the same fixed list).
var auditLogLimits = []struct{ spec, kubeone string }{
	{"maxAge", "logMaxAge"},
	{"maxBackup", "logMaxBackup"},
	{"maxSize", "logMaxSize"},
}

// auditLimitRe is a whole number from 1 to 999999999. KubeOne refuses 0
// and negative values, and nine digits cannot overflow its int.
var auditLimitRe = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)

// injectAuditLog merges spec.auditLog into the manifest as
// features.staticAuditLog: enable, the ABSOLUTE policyFilePath, and only
// the limits the spec sets (KubeOne applies its defaults to the rest).
// No-op without spec.auditLog. The spec shape and the policy file are
// checked first; a bad value stops the render with an error that names
// the value and the fix.
func (d *Driver) injectAuditLog(manifest, clusterYAML string) error {
	stderr := d.stderr()
	fail := func(format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		ui.ErrorTo(stderr, "%s", msg)
		return ui.Handled(fmt.Errorf("kubeone: %s", msg))
	}

	spec := yqsem.Load(clusterYAML)
	if !spec.OK() {
		return fail("audit log: cannot read %s", clusterYAML)
	}
	node := spec.Lookup("spec", "auditLog")
	tag := yamlTag(node)
	if tag == "!!null" {
		return nil
	}
	if tag != "!!map" {
		return fail("audit log: spec.auditLog in %s is not a mapping. Set spec.auditLog.policy to the path of an audit Policy file.", clusterYAML)
	}
	keys, _ := yqsem.MapKeys(node)
	for _, key := range keys {
		switch key {
		case "policy", "maxAge", "maxBackup", "maxSize":
		default:
			return fail("audit log: spec.auditLog.%s in %s is not a known field. Use policy, maxAge, maxBackup or maxSize.", key, clusterYAML)
		}
	}

	policyNode := yqsem.MapGet(node, "policy")
	policyTag := yamlTag(policyNode)
	if policyTag == "!!null" || (policyTag == "!!str" && yqText(policyNode) == "") {
		return fail("audit log: spec.auditLog.policy in %s is not set. Set it to the path of an audit Policy file, relative to the cluster file.", clusterYAML)
	}
	if policyTag != "!!str" {
		return fail("audit log: spec.auditLog.policy in %s is not a file path (found %s). Set it to the path of an audit Policy file, relative to the cluster file.", clusterYAML, auditShown(policyNode))
	}
	policy := yqText(policyNode)

	type limit struct{ field, value string }
	var limits []limit
	for _, l := range auditLogLimits {
		n := yqsem.MapGet(node, l.spec)
		t := yamlTag(n)
		if t == "!!null" {
			continue
		}
		if t != "!!int" || !auditLimitRe.MatchString(yqText(n)) {
			return fail("audit log: spec.auditLog.%s in %s must be a whole number from 1 to 999999999 (found %s). Fix the value, or remove the field to use the KubeOne default.", l.spec, clusterYAML, auditShown(n))
		}
		limits = append(limits, limit{l.kubeone, yqText(n)})
	}

	// The policy path is relative to the cluster file. KubeOne resolves a
	// relative path against its own manifest, which lives in .kubeone/,
	// so the manifest always gets the absolute path (bash:
	// kubeone::_abs_path, the same lexical clean).
	policyPath := policy
	if !filepath.IsAbs(policyPath) {
		policyPath = filepath.Join(filepath.Dir(clusterYAML), policyPath)
	}
	abs, err := filepath.Abs(policyPath)
	if err != nil { // only without a working directory; bash reads $PWD
		return fail("audit log: cannot resolve the policy path %s", policyPath)
	}
	policyPath = abs
	if !fsutil.IsRegular(policyPath) {
		return fail("audit log: policy file not found: %s (spec.auditLog.policy: %s). Create the file, or fix spec.auditLog.policy in %s.", policyPath, policy, clusterYAML)
	}
	if !isAuditPolicy(policyPath) {
		return fail("audit log: %s is not an audit Policy. The file must be YAML with apiVersion %s and kind %s. Fix the file, or set spec.auditLog.policy to another file.", policyPath, auditPolicyAPIVersion, auditPolicyKind)
	}

	doc, err := loadYAMLDoc(manifest)
	if err != nil {
		return fail("audit log: failed to inject features.staticAuditLog")
	}
	sal := ensureMapPath(doc, "features", "staticAuditLog")
	setKey(sal, "enable", boolNode(true))
	cfg := ensureMapPath(doc, "features", "staticAuditLog", "config")
	setKey(cfg, "policyFilePath", strNode(policyPath))
	for _, l := range limits {
		setKey(cfg, l.field, intNode(l.value))
	}
	if err := saveYAMLDoc(manifest, doc); err != nil {
		return fail("audit log: failed to inject features.staticAuditLog")
	}
	ui.DebugTo(stderr, "audit log: features.staticAuditLog injected (policy %s)", policyPath)
	return nil
}

// yamlTag is yq's `tag` on a resolved node: "!!null" for a missing node.
func yamlTag(n *yaml.Node) string {
	n = yqsem.Deref(n)
	if n == nil {
		return "!!null"
	}
	return n.Tag
}

// yqText is a scalar's text as the bash reads it through `$(yq -r …)`: the
// command substitution strips trailing newlines (a block scalar keeps one).
func yqText(n *yaml.Node) string {
	return strings.TrimRight(yqsem.Scalar(n), "\n")
}

// auditShown names a bad value in an error: the tag, and the text of a
// scalar (bash: kubeone::_audit_shown).
func auditShown(n *yaml.Node) string {
	tag := yamlTag(n)
	if tag == "!!map" || tag == "!!seq" {
		return tag
	}
	return tag + " '" + yqText(n) + "'"
}

// isAuditPolicy reports whether the first YAML document of the file is an
// audit.k8s.io/v1 Policy. It mirrors the bash check
// `yq 'select(document_index == 0) | …'`: every document must parse, and
// yq skips a document without content, so the first document is the first
// one with content.
func isAuditPolicy(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var first *yaml.Node
	for {
		doc := &yaml.Node{}
		err := dec.Decode(doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false
		}
		if first == nil && !emptyDocument(doc) {
			first = doc
		}
	}
	root := yqsem.Deref(first)
	if root == nil || root.Kind != yaml.MappingNode {
		return false
	}
	return yqsem.Scalar(yqsem.MapGet(root, "apiVersion")) == auditPolicyAPIVersion &&
		yqsem.Scalar(yqsem.MapGet(root, "kind")) == auditPolicyKind
}

// emptyDocument reports a document without content (`---` with nothing
// after it). An explicit `null` document has content.
func emptyDocument(doc *yaml.Node) bool {
	if len(doc.Content) == 0 {
		return true
	}
	root := doc.Content[0]
	return root.Kind == yaml.ScalarNode && root.Tag == "!!null" && root.Value == ""
}

func intNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: v}
}
