package lo

// nodeimage.go — spec.kubernetes.version → the kind node image tag (bash:
// spec::kind_node_version in .lok8s/utils/spec.sh). The driver, the export
// and `lo lint` read the version through this one function.

import (
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/yqsem"
)

// nodeVersionRe is the shape of a kindest/node tag: vMAJOR.MINOR.PATCH, an
// optional pre-release suffix, an optional @sha256 digest. The same pattern
// is in spec::kind_node_version.
var nodeVersionRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(@sha256:[0-9a-f]{64})?$`)

// nodeVersionHint is the second sentence of every NodeVersion problem.
const nodeVersionHint = "Use the form vMAJOR.MINOR.PATCH, with an optional @sha256:<digest>, or remove the key to use the default node image of kind."

// NodeVersion resolves spec.kubernetes.version of a Lo cluster spec to the
// tag of its kind node image (kindest/node:<tag>).
//
// It returns "" when the version is unset: the key is missing, null (`~`
// too), an empty string, or the literal string "null". For an unset version
// the kind config names no image, and kind uses the default node image of
// its own release. (`yq -r` prints "null" for a missing key and "~" for a
// tilde null; both once reached the image as kindest/node:null.)
//
// A value of a different shape is an error. Its text names the problem,
// with no prefix and no indent: the caller prints it.
func NodeVersion(root *yaml.Node) (string, error) {
	n := yqsem.Deref(yqsem.Lookup(root, "spec", "kubernetes", "version"))
	if yqsem.IsNull(n) {
		return "", nil
	}
	switch n.Kind {
	case yaml.MappingNode:
		return "", fmt.Errorf("spec.kubernetes.version is a map, not a kind node version. %s", nodeVersionHint)
	case yaml.SequenceNode:
		return "", fmt.Errorf("spec.kubernetes.version is a list, not a kind node version. %s", nodeVersionHint)
	}
	v := n.Value
	if v == "" || v == "null" {
		return "", nil
	}
	if !nodeVersionRe.MatchString(v) {
		return "", fmt.Errorf("spec.kubernetes.version '%s' is not a kind node version. %s", v, nodeVersionHint)
	}
	return v, nil
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
