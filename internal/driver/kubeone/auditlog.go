package kubeone

// auditlog.go ports the spec.auditLog half of drivers/kubeone/config
// (kubeone::_audit_log_read, kubeone::_audit_policy_verdict,
// kubeone::_inject_audit_log): spec.auditLog becomes KubeOne's
// features.staticAuditLog, so the apiserver on every control plane writes
// an audit log. KubeOne uploads the policy file to each control plane
// itself, thus the file only has to exist on this machine.

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

// auditLimitRe is a whole number from 1 to 999999999. KubeOne reads 0 as
// "not set" and applies its default, so a written 0 would hide that
// default: to get it, leave the field out. Nine digits fit KubeOne's int.
var auditLimitRe = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)

// The verdicts of the policy-file check. The bash twin
// kubeone::_audit_policy_verdict prints the same words, in the same order
// of checks, and the shared fixtures in tests/fixtures/audit-policy/ are
// named after them.
const (
	policyOK         = "ok"
	policyUnreadable = "unreadable" // the file cannot be read
	policyDirective  = "directive"  // a line starts with % (yaml.v3 and the apiserver refuse %YAML 1.2; yq reads it)
	policyMarker     = "marker"     // a value on a --- or ... line (yq v4.54 parses some of these wrongly)
	policyInvalid    = "invalid"    // a document does not parse
	policyEmptyFirst = "emptyfirst" // the first document is empty: the apiserver reads only that one
	policyDuplicate  = "duplicate"  // a mapping has a key twice
	policyAlias      = "alias"      // an anchor or alias (&name, *name): Go follows it, yq does not compare through it
	policyMerge      = "merge"      // a merge key (<<): yq follows it, a plain read does not
	policyNotPolicy  = "notpolicy"  // the first document is not an audit.k8s.io/v1 Policy
	policyNoRules    = "norules"    // no rules list, or an empty one: the apiserver refuses to start
)

// auditLogSpec is a checked spec.auditLog: the absolute policy path and
// the limits the spec sets, in auditLogLimits order.
type auditLogSpec struct {
	policyPath string
	limits     []auditLimit
}

type auditLimit struct{ field, value string }

// readAuditLog checks spec.auditLog and its policy file (bash:
// kubeone::_audit_log_read). It returns nil without spec.auditLog. A bad
// value prints an error that names the value and the fix. Provision calls
// it before the infrastructure step, so a typo fails fast; the merge calls
// it again.
func (d *Driver) readAuditLog(clusterYAML string) (*auditLogSpec, error) {
	stderr := d.stderr()
	fail := func(cause error, format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		ui.ErrorTo(stderr, "%s", msg)
		if cause != nil {
			return ui.Handled(fmt.Errorf("kubeone: %s: %w", msg, cause))
		}
		return ui.Handled(fmt.Errorf("kubeone: %s", msg))
	}

	spec := yqsem.Load(clusterYAML)
	if !spec.OK() {
		return nil, fail(spec.Err, "audit log: cannot read %s", clusterYAML)
	}
	node := spec.Lookup("spec", "auditLog")
	tag := yamlTag(node)
	if tag == "!!null" {
		return nil, nil
	}
	if tag != "!!map" {
		return nil, fail(nil, "audit log: spec.auditLog in %s is not a mapping. Set spec.auditLog.policy to the path of an audit Policy file.", clusterYAML)
	}
	keys, _ := yqsem.MapKeys(node)
	for _, key := range keys {
		switch key {
		case "policy", "maxAge", "maxBackup", "maxSize":
		default:
			return nil, fail(nil, "audit log: spec.auditLog.%s in %s is not a known field. Use policy, maxAge, maxBackup or maxSize.", key, clusterYAML)
		}
	}

	policyNode := yqsem.MapGet(node, "policy")
	policyTag := yamlTag(policyNode)
	if policyTag == "!!null" || (policyTag == "!!str" && yqText(policyNode) == "") {
		return nil, fail(nil, "audit log: spec.auditLog.policy in %s is not set. Set it to the path of an audit Policy file, relative to the cluster file.", clusterYAML)
	}
	if policyTag != "!!str" {
		return nil, fail(nil, "audit log: spec.auditLog.policy in %s is not a file path (found %s). Set it to the path of an audit Policy file, relative to the cluster file.", clusterYAML, auditShown(policyNode))
	}
	policy := yqText(policyNode)

	al := &auditLogSpec{}
	for _, l := range auditLogLimits {
		n := yqsem.MapGet(node, l.spec)
		t := yamlTag(n)
		if t == "!!null" {
			continue
		}
		if t != "!!int" || !auditLimitRe.MatchString(yqText(n)) {
			return nil, fail(nil, "audit log: spec.auditLog.%s in %s must be a whole number from 1 to 999999999 (found %s). Fix the value, or remove the field to use the KubeOne default.", l.spec, clusterYAML, auditShown(n))
		}
		al.limits = append(al.limits, auditLimit{l.kubeone, yqText(n)})
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
		return nil, fail(err, "audit log: cannot resolve the policy path %s", policyPath)
	}
	al.policyPath = abs
	if !fsutil.IsRegular(abs) {
		return nil, fail(nil, "audit log: policy file not found: %s (spec.auditLog.policy: %s). Create the file, or fix spec.auditLog.policy in %s.", abs, policy, clusterYAML)
	}
	verdict, cause := auditPolicyVerdict(abs)
	if verdict != policyOK {
		return nil, fail(cause, "%s", auditPolicyRefusal(verdict, abs))
	}
	return al, nil
}

// auditPolicyRefusal is the error line for a verdict other than ok (bash:
// the case in kubeone::_audit_log_read). Each line names the file and the
// next step.
func auditPolicyRefusal(verdict, path string) string {
	const plain = "lok8s reads a policy file only as plain YAML."
	switch verdict {
	case policyUnreadable:
		return "audit log: cannot read the policy file " + path + ". Make it readable for the user that runs lo."
	case policyDirective:
		return "audit log: " + path + " has a YAML directive (a line that starts with %). " + plain + " Remove the directive."
	case policyInvalid:
		return "audit log: " + path + " is not valid YAML. Fix the file, or set spec.auditLog.policy to another file."
	case policyMarker:
		return "audit log: " + path + " has a value on a document marker line (--- or ...). " + plain + " Move the value to the next line."
	case policyEmptyFirst:
		return "audit log: " + path + " starts with an empty YAML document. The apiserver reads only the first document and does not start. Remove the extra --- line before the policy."
	case policyDuplicate:
		return "audit log: " + path + " has a duplicate key. " + plain + " Keep each key once."
	case policyAlias:
		return "audit log: " + path + " has an anchor or an alias (&name, *name). " + plain + " Write the value out in full."
	case policyMerge:
		return "audit log: " + path + " has a merge key (<<). " + plain + " Write the keys out in full."
	case policyNoRules:
		return "audit log: " + path + " has no rules. The apiserver does not start with a policy that has no rules. Add at least one rule to the rules list."
	default: // policyNotPolicy
		return "audit log: " + path + " is not an audit Policy. The file must have apiVersion " + auditPolicyAPIVersion + " and kind " + auditPolicyKind + ". Fix the file, or set spec.auditLog.policy to another file."
	}
}

// auditPolicyVerdict checks the policy file (bash:
// kubeone::_audit_policy_verdict; the same words, the same order). The
// error is the cause for the unreadable and invalid verdicts.
//
// The first document counts, as for the apiserver: an empty first document
// is refused (the apiserver reads it as null and does not start). Where yq
// and yaml.v3 read a construct differently (a directive, an anchor or
// alias, a merge key, a duplicate key), both implementations refuse it, so
// they agree and the file means one thing.
func auditPolicyVerdict(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return policyUnreadable, err
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.HasPrefix(line, "%") {
			return policyDirective, nil
		}
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if yamlMarkerValueRe.MatchString(line) {
			return policyMarker, nil
		}
	}
	// The text rule is the one the bash twin applies, before the parse as
	// there (yq skips an empty document, so the bash cannot ask yq). An
	// empty first document that the rule does not see still fails below:
	// its root is not a mapping.
	if emptyFirstDocument(raw) {
		return policyEmptyFirst, nil
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
			return policyInvalid, err
		}
		if first == nil {
			first = doc
		}
	}
	if first == nil || len(first.Content) == 0 {
		return policyNotPolicy, nil
	}
	root := first.Content[0]
	var dup, alias, merge bool
	scanYAML(root, &dup, &alias, &merge)
	switch {
	case dup:
		return policyDuplicate, nil
	case alias:
		return policyAlias, nil
	case merge:
		return policyMerge, nil
	}
	if root.Kind != yaml.MappingNode ||
		yqsem.Scalar(yqsem.MapGet(root, "apiVersion")) != auditPolicyAPIVersion ||
		yqsem.Scalar(yqsem.MapGet(root, "kind")) != auditPolicyKind {
		return policyNotPolicy, nil
	}
	if rules := yqsem.MapGet(root, "rules"); rules == nil || rules.Kind != yaml.SequenceNode || len(rules.Content) == 0 {
		return policyNoRules, nil
	}
	return policyOK, nil
}

// scanYAML walks every node, keys included (yq: `...`), and reports a
// mapping with a key twice, an anchor or alias, and a merge key (<<). Every
// alias needs an anchor, so the bash counts anchors (yq: `anchor`); Go
// counts both.
func scanYAML(n *yaml.Node, dup, alias, merge *bool) {
	if n == nil {
		return
	}
	if n.Anchor != "" {
		*alias = true
	}
	if n.Kind == yaml.AliasNode {
		*alias = true
		return
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Tag == "!!merge" {
				*merge = true
			}
			if key.Kind == yaml.ScalarNode {
				if seen[key.Value] {
					*dup = true
				}
				seen[key.Value] = true
			}
		}
	}
	for _, c := range n.Content {
		scanYAML(c, dup, alias, merge)
	}
}

// The empty-first-document rule (bash: kubeone::_audit_empty_first). Blank
// lines and comment lines do not count. A document marker (--- or ...,
// with an optional comment) followed by another marker or by nothing is an
// empty first document. A comment or a blank line before the first --- is
// not.
var (
	yamlSkipLineRe    = regexp.MustCompile(`^\s*(#.*)?$`)
	yamlMarkerRe      = regexp.MustCompile(`^(---|\.\.\.)(\s+#.*|\s*)$`)
	yamlMarkerValueRe = regexp.MustCompile(`^(---|\.\.\.)\s+[^#\s]`)
)

func emptyFirstDocument(raw []byte) bool {
	var lines []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if yamlSkipLineRe.MatchString(line) {
			continue
		}
		lines = append(lines, line)
		if len(lines) == 2 {
			break
		}
	}
	if len(lines) == 0 || !yamlMarkerRe.MatchString(lines[0]) {
		return false
	}
	return len(lines) == 1 || yamlMarkerRe.MatchString(lines[1])
}

// injectAuditLog merges spec.auditLog into the manifest as
// features.staticAuditLog: enable, the ABSOLUTE policyFilePath, and only
// the limits the spec sets (KubeOne applies its defaults to the rest).
// No-op without spec.auditLog.
func (d *Driver) injectAuditLog(manifest, clusterYAML string) error {
	al, err := d.readAuditLog(clusterYAML)
	if err != nil || al == nil {
		return err
	}
	// Name the cause: a manifest lok8s cannot read is a permission problem,
	// one it cannot parse is a template problem, a write that fails is a
	// disk or permission problem (bash: the same three lines).
	fail := func(msg string, cause error) error {
		ui.ErrorTo(d.stderr(), "%s", msg)
		return ui.Handled(fmt.Errorf("kubeone: %s: %w", msg, cause))
	}
	dir := filepath.Dir(manifest)
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return fail("audit log: lok8s cannot read the rendered manifest in "+dir+". Check the permissions of the directory, then run lo provision again.", err)
	}
	doc, err := parseYAMLDoc(raw)
	if err != nil {
		return fail("audit log: lok8s cannot parse the rendered manifest in "+dir+" as YAML. Check the KubeOne template (drivers/kubeone/cluster/core/kubeone.yaml), then run lo provision again.", err)
	}
	sal := ensureMapPath(doc, "features", "staticAuditLog")
	setKey(sal, "enable", boolNode(true))
	cfg := ensureMapPath(doc, "features", "staticAuditLog", "config")
	setKey(cfg, "policyFilePath", strNode(al.policyPath))
	for _, l := range al.limits {
		setKey(cfg, l.field, intNode(l.value))
	}
	if err := saveYAMLDoc(manifest, doc); err != nil {
		return fail("audit log: cannot write the manifest in "+dir+". Check the free disk space and the permissions of the directory, then run lo provision again.", err)
	}
	ui.DebugTo(d.stderr(), "audit log: features.staticAuditLog injected (policy %s)", al.policyPath)
	return nil
}

// yamlTag is yq's `tag` on a resolved node: "!!null" for a missing node.
// It is not in yqsem: the private tag helpers of bootstrapspec (by kind),
// addons (no deref, ShortTag) and lint (the JSON round trip) each read a
// tag in another way, so one shared helper would change two of them.
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
