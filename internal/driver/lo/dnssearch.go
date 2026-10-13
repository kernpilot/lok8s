package lo

// dnssearch.go — the spec.network.dnsSearch check, shared by the read pass
// (readDNSSearchConfig) and `lo lint` (bash: dnssearch::check in
// .lok8s/utils/dnssearch.sh). The reason for the key is at
// renderKindConfig. Both implementations print the same lines in the same
// order: keep the checks, their order and the wording in step.

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/yqsem"
)

// MaxDNSSearch is the most entries spec.network.dnsSearch takes: the
// Kubernetes limit for the search list of a pod (MaxDNSSearchPaths).
const MaxDNSSearch = 32

// dnsCharsRe is the character set of a DNS name. The bash twin runs the
// same expression through yq's test(), the same Go regexp engine, so both
// decide alike on non-ASCII text, control characters and newlines.
var dnsCharsRe = regexp.MustCompile(`^[A-Za-z0-9.-]*$`)

// CheckDNSSearch checks spec.network.dnsSearch of a cluster spec. It
// returns the names as written, or one problem line per bad entry (no
// prefix; the caller adds `error: ` or the lint indent). A missing key, a
// null and a spec.network that is not a map give no names and no problems.
// DNS is case-insensitive and a trailing dot marks a fully qualified name,
// so both are valid. A spec with a problem returns no names. Aliases
// resolve one level per node (the list, each entry), never the whole tree:
// an alias cycle inside the list reads as "a list", and one anywhere else
// costs nothing.
func CheckDNSSearch(root *yaml.Node) (names, problems []string) {
	// Merge keys on the path resolve as in yq with
	// --yaml-fix-merge-anchor-to-spec (the bash twin's flag).
	n := yqsem.LookupMerged(root, "spec", "network", "dnsSearch")
	if yqsem.IsNull(n) {
		return nil, nil
	}
	if n.Kind != yaml.SequenceNode {
		return nil, []string{"spec.network.dnsSearch must be a list of DNS names. Found " +
			describeYAMLValue(n) + ". Write a YAML list, for example [corp.example.com]."}
	}
	items := yqsem.SeqItems(n)
	if len(items) > MaxDNSSearch {
		extra := len(items) - MaxDNSSearch
		return nil, []string{fmt.Sprintf("spec.network.dnsSearch has %d entries. The limit is %d. Remove %d %s.",
			len(items), MaxDNSSearch, extra, plural(extra, "entry", "entries"))}
	}
	for i, item := range items {
		if p := dnsSearchEntryProblem(item); p != "" {
			problems = append(problems, fmt.Sprintf("spec.network.dnsSearch[%d] %s", i, p))
			continue
		}
		names = append(names, item.Value)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return names, nil
}

// dnsSearchEntryProblem returns the problem of one entry ("" for a valid
// name): the first failed check, in this order, with one next action per
// cause.
func dnsSearchEntryProblem(item *yaml.Node) string {
	if yqsem.IsNull(item) || (item.ShortTag() == "!!str" && item.Value == "") {
		return "is empty. Write a DNS name, or remove the entry."
	}
	if item.ShortTag() != "!!str" {
		return "must be a DNS name. Found " + describeYAMLValue(item) +
			". Write the entry as text, for example corp.example.com."
	}
	v := item.Value
	// Its own cause: the bash twin finds a NUL inside yq (a $(…) capture
	// drops the byte), and the value is not printed.
	if strings.ContainsRune(v, 0) {
		return "has a NUL character. Remove the NUL character."
	}
	if !dnsCharsRe.MatchString(v) {
		return "has a character that is not valid in a DNS name: '" + v + "'. Use only letters, digits, hyphens and dots."
	}
	name := strings.TrimSuffix(v, ".")
	if len(name) > 253 {
		return fmt.Sprintf("has %d characters: '%s'. The limit is 253, without a trailing dot. Use a shorter name.", len(name), v)
	}
	for label := range strings.SplitSeq(name, ".") {
		switch {
		case label == "":
			return "has an empty label: '" + v + "'. Remove the extra dot."
		case len(label) > 63:
			return fmt.Sprintf("has a label of %d characters: '%s'. The limit is 63 characters per label. Use a shorter label.", len(label), v)
		case label[0] == '-' || label[len(label)-1] == '-':
			return "has a label that starts or ends with a hyphen: '" + v + "'. Remove that hyphen."
		}
	}
	return ""
}

// describeYAMLValue names a YAML value for an error line in plain words
// (no YAML tags for the common kinds).
func describeYAMLValue(n *yaml.Node) string {
	switch tag := n.ShortTag(); tag {
	case "!!str":
		return "the text '" + n.Value + "'"
	case "!!int", "!!float":
		return "the number " + n.Value
	case "!!bool":
		return "the boolean " + n.Value
	case "!!map":
		return "a map"
	case "!!seq":
		return "a list"
	default:
		return "a value with the tag " + tag
	}
}

// plural picks the singular for exactly one.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
