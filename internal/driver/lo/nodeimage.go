package lo

// nodeimage.go — spec.kubernetes.version → the kind node image tag (bash:
// spec::kind_node_version in .lok8s/utils/spec.sh). The driver and `lo lint`
// read the version through this one function. The spec export
// (exportSpecEnvs) does not: it must not stop a command that does not use
// the version, so it reads the raw value and only maps "null" to "".

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

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

// nodeVersionUnreadable starts every NodeVersion problem about the shape of
// the spec file.
const nodeVersionUnreadable = "spec.kubernetes.version cannot be read: "

// versionProblem is a NodeVersion error: full sentences for the user, which
// the caller prints as they are. They end with a period, so they are built
// with fmt.Sprintf and not as error-string literals.
type versionProblem string

func (p versionProblem) Error() string { return string(p) }

// unreadable is a problem about the shape of the spec file.
func unreadable(format string, args ...any) error {
	return versionProblem(nodeVersionUnreadable + fmt.Sprintf(format, args...))
}

// NodeVersion reads spec.kubernetes.version of a Lo cluster spec file and
// returns the tag of its kind node image (kindest/node:<tag>).
//
// It reads the file as yq does in the bash twin: aliases and merge keys
// resolved (`explode(.)` under --yaml-fix-merge-anchor-to-spec), trailing
// newlines of the value removed (bash command substitution removes them).
//
// It returns "" when the version is unset: spec, spec.kubernetes or the key
// is missing or null (`~` too), or the value is an empty string or the
// literal string "null". For an unset version the kind config names no
// image, and kind uses the default node image of its own release.
//
// A shape that can hold a version but cannot be read is an error, never
// unset: a file with more than one YAML document, a merge key that yq
// refuses, a spec or spec.kubernetes that is not a map, a version that is a
// map or a list. A scalar that is not an image tag is an error too. The
// error text has no prefix and no indent: the caller prints it.
func NodeVersion(clusterYAML string) (string, error) {
	docs, err := yqsem.LoadDocs(clusterYAML)
	if err != nil {
		return "", unreadable("%v. Correct the spec file.", err)
	}
	if len(docs) > 1 {
		return "", unreadable("the spec file has %d YAML documents. Keep only the cluster spec in the file.", len(docs))
	}
	var n *yaml.Node
	if len(docs) == 1 {
		n = docs[0]
	}
	switch err := yqsem.CheckMerges(n); {
	case errors.Is(err, yqsem.ErrMergeValue):
		return "", unreadable("a merge key (<<) in the spec file does not refer to a map or a list of maps. Correct the merge key.")
	case errors.Is(err, yqsem.ErrMergeCycle):
		// yq overflows its stack here: the bash twin refuses with the
		// first line of the yq output instead.
		return "", unreadable("a merge key (<<) in the spec file refers back to its own map. Correct the merge key.")
	}

	// Each hop: the name of the node in a problem, and the key read from it.
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
		n, _ = yqsem.MapGetMerged(n, hop.key)
	}
	if yqsem.IsNull(n) {
		return "", nil
	}
	if n.Kind != yaml.ScalarNode {
		return "", unreadable("the value is %s, not an image tag. %s", shapeOf(n), nodeVersionHint)
	}
	v := strings.TrimRight(n.Value, "\n")
	if v == "" || v == "null" {
		return "", nil
	}
	if !nodeVersionRe.MatchString(v) {
		return "", versionProblem(fmt.Sprintf("spec.kubernetes.version '%s' is not a valid image tag. %s", v, nodeVersionHint))
	}
	return v, nil
}

// shapeOf names a node in a NodeVersion problem: "a map", "a list", or the
// quoted text of a scalar.
func shapeOf(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a map"
	case yaml.SequenceNode:
		return "a list"
	}
	return "'" + strings.TrimRight(n.Value, "\n") + "'"
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
