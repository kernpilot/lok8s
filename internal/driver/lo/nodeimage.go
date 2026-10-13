package lo

// nodeimage.go — spec.kubernetes.version → the kind node image tag (bash:
// spec::kind_node_version in .lok8s/utils/spec.sh). The driver and `lo lint`
// read the version through NodeVersion, and `lo audit` through
// KubernetesVersion, the read that NodeVersion starts with. The spec export
// (exportSpecEnvs) does not: it must not stop a command that does not use
// the version, so it reads the raw value and only maps "null" to "".

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// nodeVersionRe is an OCI image tag (the distribution reference grammar:
// 1 to 128 characters, no leading '.' or '-'), with an optional @sha256
// digest. The version is only the tag of kindest/node:<tag>, so any tag
// is valid: `latest` and the tags of `kind build node-image` too. The same
// pattern is in spec::kind_node_version.
var nodeVersionRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}(@sha256:[0-9a-f]{64})?$`)

// nodeVersionHint is the last sentence of a NodeVersion problem about the
// value.
const nodeVersionHint = "Set a kindest/node image tag, for example v1.31.4. To use the default node image of kind, remove the key."

// versionUnreadable starts every problem about a spec that the reader
// cannot read.
const versionUnreadable = "spec.kubernetes.version cannot be read: "

// controlRe and longRe shape a value that a problem shows (displayValue).
// The bash twin runs the same two expressions in yq, which uses the same Go
// regexp engine. controlRe is every control character: the C0 and C1
// controls (\p{Cc}: NUL, ESC, U+0085, U+009B …), the format characters
// (\p{Cf}: the bidi overrides …) and the line and paragraph separators
// (U+2028, U+2029).
var (
	controlRe = regexp.MustCompile(`[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]`)
	longRe    = regexp.MustCompile(`^(.{80}).+$`)
)

// maxMergeReads is the most mapping reads that the merge keys of one
// mapping on the path may cost (yqsem.MergeReads). Bash twin:
// _SPEC_KVER_MAX_READS.
const maxMergeReads = 10000

// versionProblem is a reader error: full sentences for the user, which the
// caller prints as they are. They end with a period, so they are built
// with fmt.Sprintf and not as error-string literals.
type versionProblem string

func (p versionProblem) Error() string { return string(p) }

// unreadable is a problem about a spec that the reader cannot read.
func unreadable(format string, args ...any) error {
	return versionProblem(versionUnreadable + fmt.Sprintf(format, args...))
}

// displayValue is a value as a problem shows it: each control character
// as '?', and at most 80 characters, then "...". A NUL byte or an escape
// sequence never reaches the terminal.
func displayValue(v string) string {
	return longRe.ReplaceAllString(controlRe.ReplaceAllString(v, "?"), "${1}...")
}

// KubernetesVersion reads spec.kubernetes.version of a cluster spec file
// with the rules that every driver shares (bash: spec::kubernetes_version
// in .lok8s/utils/spec.sh). It returns the text of the value, or "" when
// the version is unset: spec, spec.kubernetes or the key is missing or null
// (`~` too), or the value is an empty string or the literal string "null".
//
// It reads the file as a yq path read does in the bash twin (yq
// --yaml-fix-merge-anchor-to-spec; yqsem.MapGetMerged): aliases and merge
// keys resolved one key at a time, nothing else expanded, so an alias bomb
// or an alias cycle off the path costs nothing. The trailing newlines of
// the value are removed (bash command substitution removes them).
//
// A shape that can hold a version but cannot be read is an error, never
// unset: a layout other than one YAML document (domain.SpecLayout), a merge
// key anywhere in the file that does not refer to a map or a list of maps
// (yq would skip it), merge keys on the path that form a cycle or need more
// than maxMergeReads map reads, a spec or spec.kubernetes that is not a
// map, a version that is a map or a list, a value with a control character
// (bash would drop a NUL byte). The error text has no
// prefix and no indent: the caller prints it.
func KubernetesVersion(specFile string) (string, error) {
	if problem, ok := domain.SpecLayout(specFile); !ok {
		return "", unreadable("the spec file %s", problem)
	}
	docs, err := yqsem.LoadDocs(specFile)
	if err != nil {
		return "", unreadable("%v. Correct the spec file.", err)
	}
	var n *yaml.Node
	if len(docs) > 0 {
		n = docs[0]
	}
	if yqsem.CheckMerges(n) != nil {
		return "", unreadable("a merge key (<<) in the spec file does not refer to a map or a list of maps. Correct the merge key.")
	}

	// Each hop: the name of the node in a problem, and the key read from it.
	// Before a mapping is read, its merge keys are counted
	// (yqsem.MergeReads): yq reads a mapping again for each merge key that
	// names it, so a doubling chain would cost the bash twin minutes.
	n = yqsem.Deref(n)
	for _, hop := range [...]struct{ name, key string }{
		{"the spec file", "spec"}, {"spec", "kubernetes"}, {"spec.kubernetes", "version"},
	} {
		if yqsem.IsNull(n) {
			return "", nil
		}
		if n.Kind != yaml.MappingNode {
			return "", unreadable("%s is %s, not a map. Write %s as a YAML map.", hop.name, shapeOf(n), hop.name)
		}
		reads, err := yqsem.MergeReads(n, maxMergeReads)
		if err != nil {
			return "", unreadable("the merge keys (<<) on the path to the value form a cycle. Remove the cycle.")
		}
		if reads > maxMergeReads {
			return "", unreadable("the merge keys (<<) on the path to the value need more than %d map reads. Merge fewer maps.", maxMergeReads)
		}
		if n, _, err = yqsem.MapGetMerged(n, hop.key); err != nil {
			return "", unreadable("the merge keys (<<) on the path to the value form a cycle. Remove the cycle.")
		}
	}
	if yqsem.IsNull(n) {
		return "", nil
	}
	if n.Kind != yaml.ScalarNode {
		return "", unreadable("the value is %s, not a single value. Write spec.kubernetes.version as a single value.", shapeOf(n))
	}
	v := strings.TrimRight(n.Value, "\n")
	if controlRe.MatchString(v) {
		return "", versionProblem(fmt.Sprintf("spec.kubernetes.version '%s' contains a control character (shown as '?'). Remove the control character.", displayValue(v)))
	}
	if v == "" || v == "null" {
		return "", nil
	}
	return v, nil
}

// NodeVersion reads spec.kubernetes.version of a Lo cluster spec file and
// returns the tag of its kind node image (kindest/node:<tag>), "" when the
// version is unset (KubernetesVersion). For an unset version the kind
// config names no image, and kind uses the default node image of its own
// release. A value that is not an image tag is an error.
func NodeVersion(clusterYAML string) (string, error) {
	v, err := KubernetesVersion(clusterYAML)
	if err != nil || v == "" {
		return "", err
	}
	if !nodeVersionRe.MatchString(v) {
		return "", versionProblem(fmt.Sprintf("spec.kubernetes.version '%s' is not a valid image tag. %s", displayValue(v), nodeVersionHint))
	}
	return v, nil
}

// shapeOf names a node in a problem: "a map", "a list", or the quoted
// text of a scalar (displayValue).
func shapeOf(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a map"
	case yaml.SequenceNode:
		return "a list"
	}
	return "'" + displayValue(strings.TrimRight(n.Value, "\n")) + "'"
}

// nodeImageLine is the `image:` line of one node in the kind config, with
// its leading newline. It is empty for an unset version, so that kind
// selects its default node image. The "null" guard keeps a raw yq read from
// ever rendering kindest/node:null.
func nodeImageLine(k8sVersion string) string {
	if k8sVersion == "" || k8sVersion == "null" {
		return ""
	}
	return "\n    image: \"kindest/node:" + k8sVersion + "\""
}
