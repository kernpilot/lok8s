package secrets

// sopsrules.go: the YAML read of .sops.yaml behind remove-key (bash:
// secrets::_rules_form, secrets::_rules_hits and secrets::_rules_check,
// through yq). The edit in dropRecipient works on lines. These checks parse
// the file, so that a recipient in an escaped string, a YAML list, an alias,
// a merge key or a key group cannot stay in a rule while the edit reports it
// removed.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	sopsconfig "github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/keys"
	"gopkg.in/yaml.v3"
)

// splitRecipients is sops' reading of an age string: split on ",", each part
// trimmed, empty parts dropped.
func splitRecipients(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// countRecipient counts the recipient in an age string.
func countRecipient(s, recipient string) int {
	n := 0
	for _, r := range splitRecipients(s) {
		if r == recipient {
			n++
		}
	}
	return n
}

// encodedAgeHits counts the recipient in the age values of the creation rules
// that the line edit does not rewrite: a plain or double-quoted string (its
// escapes decoded) and the strings of a YAML list. It reads each rule's own
// age key as written; a single-quoted string is the one the edit rewrites,
// and an alias or a merge key is left to the check of the edited file.
func encodedAgeHits(doc *yaml.Node, recipient string) int {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return 0
	}
	rules := mappingValue(doc.Content[0], "creation_rules")
	if rules == nil || rules.Kind != yaml.SequenceNode {
		return 0
	}
	hits := 0
	for _, rule := range rules.Content {
		age := mappingValue(rule, "age")
		if age == nil {
			continue
		}
		switch age.Kind {
		case yaml.ScalarNode:
			if age.ShortTag() == "!!str" && age.Style&yaml.SingleQuotedStyle == 0 {
				hits += countRecipient(age.Value, recipient)
			}
		case yaml.SequenceNode:
			for _, item := range age.Content {
				if item.Kind == yaml.ScalarNode && item.ShortTag() == "!!str" {
					hits += countRecipient(item.Value, recipient)
				}
			}
		}
	}
	return hits
}

// yamlForm is what yamlFormCheck found in .sops.yaml.
type yamlForm int

const (
	formOK  yamlForm = iota
	formTag          // an explicit tag other than !!str, !!seq or !!map under creation_rules
	formKey          // a mapping key that is not a string
)

// yamlFormCheck refuses, it does not interpret (bash: secrets::_rules_form).
// It finds a node under creation_rules (aliases and merge keys followed) that
// carries an explicit tag other than !!str, !!seq or !!map, and a mapping key
// anywhere in the file whose tag is not !!str: an int, a bool or a null key,
// implicit or not. yaml.v3 decodes a !!binary value to its bytes where yq
// keeps it tagged, and decodes a mapping with a key that is not a string as
// map[any]any, which the rule reads skip: either would let a recipient stay
// in a rule that one twin does not read. A merge key (<<) is allowed; the
// mappings it merges are checked like any other.
func yamlFormCheck(doc *yaml.Node) yamlForm {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return formOK
	}
	root := doc.Content[0]
	tags := &formWalk{seen: map[*yaml.Node]bool{}}
	for _, rules := range creationRules(root, map[*yaml.Node]bool{}) {
		tags.walk(rules)
	}
	if tags.tag {
		return formTag
	}
	keys := &formWalk{seen: map[*yaml.Node]bool{}}
	keys.walk(root)
	if keys.key {
		return formKey
	}
	return formOK
}

// formWalk visits a node and every value below it once, aliases followed. It
// notes a node with an explicit tag other than !!str, !!seq or !!map (tag),
// and a mapping key whose tag is not !!str or !!merge (key).
type formWalk struct {
	seen     map[*yaml.Node]bool
	tag, key bool
}

func (w *formWalk) walk(n *yaml.Node) {
	if n == nil || w.seen[n] {
		return
	}
	w.seen[n] = true
	if n.Kind == yaml.AliasNode {
		w.walk(n.Alias)
		return
	}
	if n.Style&yaml.TaggedStyle != 0 {
		switch n.ShortTag() {
		case "!!str", "!!seq", "!!map":
		default:
			w.tag = true
		}
	}
	for i, child := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 {
			if t := child.ShortTag(); t != "!!str" && t != "!!merge" {
				w.key = true
			}
			continue
		}
		w.walk(child)
	}
}

// creationRules is the value of each creation_rules key of the top mapping,
// a key that a merge key brings in included.
func creationRules(n *yaml.Node, seen map[*yaml.Node]bool) []*yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n == nil || seen[n] {
		return nil
	}
	seen[n] = true
	var out []*yaml.Node
	switch n.Kind {
	case yaml.SequenceNode: // the value of a merge key can list mappings
		for _, item := range n.Content {
			out = append(out, creationRules(item, seen)...)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			switch {
			case k.ShortTag() == "!!merge":
				out = append(out, creationRules(v, seen)...)
			case k.Kind == yaml.ScalarNode && k.Value == "creation_rules":
				out = append(out, v)
			}
		}
	}
	return out
}

// mappingValue is the value of key in a mapping node, as written: nil for a
// node that is not a mapping or a key it does not hold.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// decodedRules is the creation_rules list of a .sops.yaml with aliases and
// merge keys resolved. err is a parse error.
func decodedRules(raw []byte) ([]any, error) {
	var top any
	if err := yaml.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	m, _ := top.(map[string]any)
	rules, _ := m["creation_rules"].([]any)
	return rules, nil
}

// ruleHits counts the recipient in the decoded creation rules: the age list
// of every mapping under a rule's key_groups (merge groups included) and,
// unless groupsOnly, the rule's own age.
func ruleHits(rules []any, recipient string, groupsOnly bool) int {
	hits := 0
	for _, r := range rules {
		rule, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if !groupsOnly {
			hits += ageValueHits(rule["age"], recipient)
		}
		if groups, ok := rule["key_groups"].([]any); ok {
			for _, g := range groups {
				hits += groupHits(g, recipient)
			}
		}
	}
	return hits
}

// groupHits counts the recipient in the age value of every mapping in v, at
// any depth.
func groupHits(v any, recipient string) int {
	hits := 0
	switch t := v.(type) {
	case map[string]any:
		hits += ageValueHits(t["age"], recipient)
		for _, sub := range t {
			hits += groupHits(sub, recipient)
		}
	case []any:
		for _, sub := range t {
			hits += groupHits(sub, recipient)
		}
	}
	return hits
}

// ageValueHits counts the recipient in a decoded age value: a string, or the
// strings of a list (nested lists included).
func ageValueHits(v any, recipient string) int {
	switch t := v.(type) {
	case string:
		return countRecipient(t, recipient)
	case []any:
		hits := 0
		for _, item := range t {
			hits += ageValueHits(item, recipient)
		}
		return hits
	}
	return 0
}

// ruleProblem is what the rule check found for one .enc file.
type ruleProblem int

const (
	ruleOK ruleProblem = iota
	ruleNone
	ruleNotAge
	ruleBadAge
)

// ruleCheck is the result of checkRules: the .enc files that match no rule,
// those whose rule has a recipient that is not age, and those whose rule has
// an age recipient that does not parse.
type ruleCheck struct {
	none, notAge, badAge []string
}

// checkRules matches the plaintext twin of each .enc file against the
// creation rules in content, the way sops does when it encrypts: the path
// relative to the directory of .sops.yaml, the first rule whose path_regex
// matches. It sorts out the files that match no rule, those whose rule has a
// recipient that is not age (this build encrypts with age only) and those
// whose rule has an age recipient that does not parse. err is set when sops
// cannot read the rules at all (a bad path_regex, a value of the wrong type).
// The rules are read from a copy in a temp directory: sops reads a config
// from a file only, and the project is not written before the checks pass.
func checkRules(configPath, content string, encs []string) (ruleCheck, error) {
	var found ruleCheck
	configDir, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return found, err
	}
	tmp, err := os.MkdirTemp("", "lo-sops-rules-")
	if err != nil {
		return found, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	copyPath := filepath.Join(tmp, ".sops.yaml")
	if err := os.WriteFile(copyPath, []byte(content), 0o600); err != nil {
		return found, err
	}
	for _, enc := range encs {
		plain, err := filepath.Abs(strings.TrimSuffix(enc, ".enc"))
		if err != nil {
			return found, err
		}
		rel := strings.TrimPrefix(plain, configDir+string(filepath.Separator))
		switch problem, err := ruleFor(copyPath, rel); {
		case err != nil:
			return found, err
		case problem == ruleNone:
			found.none = append(found.none, enc)
		case problem == ruleNotAge:
			found.notAge = append(found.notAge, enc)
		case problem == ruleBadAge:
			found.badAge = append(found.badAge, enc)
		}
	}
	return found, nil
}

// ruleFor classifies the creation rule sops picks for rel. The error texts
// are the ones of the pinned sops fork. An age error is never printed: the
// value it quotes can be a private key that was pasted into .sops.yaml.
func ruleFor(configPath, rel string) (ruleProblem, error) {
	cfg, err := sopsconfig.LoadCreationRuleForFile(configPath, rel, nil)
	var unsupported *keys.UnsupportedKeyTypeError
	switch {
	case errors.As(err, &unsupported):
		return ruleNotAge, nil
	case err != nil && strings.Contains(err.Error(), "no matching creation rules found"):
		return ruleNone, nil
	case err != nil && ageRecipientError(err):
		return ruleBadAge, nil
	case err != nil:
		return ruleOK, err
	case cfg == nil:
		return ruleNone, nil
	}
	return ruleOK, nil
}

// ageRecipientError reports an error of the sops loader about an age
// recipient of the matched rule: one that does not parse (age's
// parseRecipient: "failed to parse input …"), or an item of an age list that
// is not a string (parseKeyField: "invalid age key configuration: …").
func ageRecipientError(err error) bool {
	msg := err.Error()
	return strings.HasPrefix(msg, "failed to parse input") || strings.HasPrefix(msg, "invalid age key configuration")
}
