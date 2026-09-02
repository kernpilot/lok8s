package audit

// Envoy Gateway SecurityPolicy posture (bash: audit::_secpol_scan) and the
// EncryptionConfiguration proof (bash: audit::_encryption_encrypts_secrets).
// Read the bash originals for the full design rationale — the semantics here
// are a 1:1 transcription, per YAML OBJECT rather than per file:
//
//	denyPolicies — SecurityPolicies with `authorization.defaultAction: Deny`
//	               that do NOT target a route (Gateway / listener /
//	               ListenerSet, or untargeted) — the gateway-wide carve-out.
//	openPolicies — SecurityPolicies that DO target a route (HTTPRoute,
//	               GRPCRoute or TCPRoute), do NOT merge (only StrategicMerge
//	               and JSONMerge count — the CRD has no enum, so any other
//	               value is accepted by admission and merges NOTHING), and do
//	               NOT re-deny by themselves — each one cancels the carve-out
//	               for its routes.
//
// "Denies" requires more than declaring Deny: an Allow rule that re-opens the
// policy to EVERY client (narrowed by nothing but a /0 CIDR) makes the
// declaration cosmetic. The allow-all predicate is deliberately narrow — an
// `operation`-scoped rule or a jwt/headers principal beside the CIDR is a
// real guard, and flagging it would fire on a safe gateway. Matching the /0
// PREFIX (not the literals 0.0.0.0/0 / ::/0) is not pedantry: `10.0.0.0/0`
// is every IPv4 address while reading like an RFC1918 range.
//
// The routed set stops at HTTPRoute/GRPCRoute/TCPRoute ON PURPOSE —
// SecurityPolicy cannot attach a mergeType anywhere else, so wider kinds
// would manufacture findings against policies that enforce nothing either
// way (see the bash comment block for the Envoy Gateway v1.9 verification).
//
// LIMIT (named in the finding): the two counts are not correlated by gateway
// attachment, so the check over-reports and never under-reports.

import (
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

var reZeroPrefix = regexp.MustCompile(`/0$`)

// secpolScan walks every SecurityPolicy object in the files and returns the
// deny/open counts. Unparsable input yields no objects and is counted as
// neither — a file we cannot read must never manufacture a carve-out. The
// parse is STREAMING per document (like yq): documents before a syntax error
// still count.
func secpolScan(files []string) (denyPolicies, openPolicies int) {
	for _, f := range files {
		docs, err := decodeDocs(f, true)
		if err != nil {
			continue
		}
		for _, doc := range docs {
			if yqRenderNode(mapValue(doc, "kind")) != "SecurityPolicy" {
				continue
			}
			action := altNode(lookupPath(doc, "spec", "authorization", "defaultAction"), "-")
			merge := altNode(lookupPath(doc, "spec", "mergeType"), "-")
			routed := false
			for _, k := range targetKinds(doc) {
				if k == "HTTPRoute" || k == "GRPCRoute" || k == "TCPRoute" {
					routed = true
					break
				}
			}
			merges := merge == "StrategicMerge" || merge == "JSONMerge"
			// One definition of "denies", used for BOTH counts on purpose (a
			// Deny that allow-alls is no more a gateway-wide carve-out than a
			// safe route policy).
			denies := action == "Deny" && allowAllRules(doc) == 0
			if routed {
				// No usable mergeType means this policy REPLACES the
				// gateway-wide one for the routes it selects, whatever it
				// declares. It is safe only if what replaces the carve-out is
				// itself a real deny.
				if !merges && !denies {
					openPolicies++
				}
			} else if denies {
				denyPolicies++
			}
		}
	}
	return denyPolicies, openPolicies
}

// targetKinds collects the .kind of every map-shaped target across
// targetRef + targetRefs + targetSelectors (yq: `[.spec.targetRef] +
// (.spec.targetRefs // []) + (.spec.targetSelectors // []) | map(select(tag
// == "!!map") | .kind // "-")`).
func targetKinds(doc *yaml.Node) []string {
	var candidates []*yaml.Node
	candidates = append(candidates, lookupPath(doc, "spec", "targetRef"))
	for _, key := range []string{"targetRefs", "targetSelectors"} {
		if seq := lookupPath(doc, "spec", key); seq != nil && seq.Kind == yaml.SequenceNode {
			for _, item := range seq.Content {
				candidates = append(candidates, resolveNode(item))
			}
		}
	}
	var kinds []string
	for _, c := range candidates {
		if c == nil || c.Kind != yaml.MappingNode {
			continue
		}
		kinds = append(kinds, altNode(mapValue(c, "kind"), "-"))
	}
	return kinds
}

// allowAllRules counts the Allow rules (per matching /0 CIDR, like the yq
// collect) in .spec.authorization.rules that re-open the policy to EVERY
// client: action Allow, keys ⊆ {name, action, principal} (an unknown key
// means NOT counted, so the failure direction is a missed finding, never a
// false alarm), principal keys exactly {clientCIDRs}, and some CIDR with a
// ZERO prefix length.
func allowAllRules(doc *yaml.Node) int {
	rules := lookupPath(doc, "spec", "authorization", "rules")
	if rules == nil || rules.Kind != yaml.SequenceNode {
		return 0
	}
	n := 0
	for _, item := range rules.Content {
		rule := resolveNode(item)
		if rule == nil || rule.Kind != yaml.MappingNode {
			continue
		}
		if yqRenderNode(mapValue(rule, "action")) != "Allow" {
			continue
		}
		subset := true
		for i := 0; i+1 < len(rule.Content); i += 2 {
			k := resolveNode(rule.Content[i])
			if k == nil {
				continue
			}
			if k.Value != "action" && k.Value != "name" && k.Value != "principal" {
				subset = false
				break
			}
		}
		if !subset {
			continue
		}
		principal := mapValue(rule, "principal")
		if principal == nil || principal.Kind != yaml.MappingNode {
			continue
		}
		var keys []string
		for i := 0; i+1 < len(principal.Content); i += 2 {
			if k := resolveNode(principal.Content[i]); k != nil {
				keys = append(keys, k.Value)
			}
		}
		sort.Strings(keys)
		if len(keys) != 1 || keys[0] != "clientCIDRs" {
			continue
		}
		cidrs := mapValue(principal, "clientCIDRs")
		if cidrs == nil || cidrs.Kind != yaml.SequenceNode {
			continue
		}
		for _, c := range cidrs.Content {
			cn := resolveNode(c)
			if cn != nil && cn.Kind == yaml.ScalarNode && reZeroPrefix.MatchString(cn.Value) {
				n++
			}
		}
	}
	return n
}

// encryptionEncryptsSecrets reports whether a rendered
// EncryptionConfiguration uses a REAL provider (aescbc/aesgcm/secretbox/kms)
// as the WRITE (first) provider of the FIRST resource group covering
// `secrets` — the bare `secrets` resource, or the `*.` (core group) / `*.*`
// (all) wildcards. FIRST covering group only, because that is the
// apiserver's precedence: the first resources entry matching a resource wins
// — `secrets → identity` listed BEFORE `*.* → aescbc` writes Secrets
// PLAINTEXT, and the later covering group must never rescue the verdict.
// Fail-CLOSED on unreadable input: a parse-error file yields no proof (the
// caller, which already knows the config EXISTS, reports FAIL, not unknown).
func encryptionEncryptsSecrets(files []string) bool {
	for _, f := range files {
		// yq eval-all collects across the WHOLE file before emitting, so any
		// parse error drops the entire file (partial=false).
		docs, err := decodeDocs(f, false)
		if err != nil {
			continue
		}
		var group *yaml.Node
		for _, doc := range docs {
			if yqRenderNode(mapValue(doc, "kind")) != "EncryptionConfiguration" {
				continue
			}
			resources := mapValue(doc, "resources")
			if resources == nil || resources.Kind != yaml.SequenceNode {
				continue
			}
			for _, g := range resources.Content {
				gn := resolveNode(g)
				if coversSecrets(gn) {
					group = gn
					break
				}
			}
			if group != nil {
				break
			}
		}
		if group == nil {
			continue
		}
		providers := mapValue(group, "providers")
		if providers == nil || providers.Kind != yaml.SequenceNode || len(providers.Content) == 0 {
			continue
		}
		first := resolveNode(providers.Content[0])
		if first == nil || first.Kind != yaml.MappingNode || len(first.Content) < 2 {
			continue
		}
		writer := resolveNode(first.Content[0])
		if writer == nil {
			continue
		}
		switch writer.Value {
		case "aescbc", "aesgcm", "secretbox", "kms", "kmsv2":
			return true
		}
	}
	return false
}

// coversSecrets reports whether a resource group's `resources:` list names
// secrets or a covering wildcard.
func coversSecrets(group *yaml.Node) bool {
	rs := mapValue(group, "resources")
	if rs == nil || rs.Kind != yaml.SequenceNode {
		return false
	}
	for _, r := range rs.Content {
		rn := resolveNode(r)
		if rn == nil || rn.Kind != yaml.ScalarNode {
			continue
		}
		switch rn.Value {
		case "secrets", "*.*", "*.":
			return true
		}
	}
	return false
}
