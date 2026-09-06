package audit

import (
	"path/filepath"
	"testing"
)

func scanFixture(t *testing.T, content string) (deny, open int) {
	t.Helper()
	dir := t.TempDir()
	f := filepath.Join(dir, "p.yaml")
	writeFileT(t, f, content)
	return secpolScan([]string{f})
}

func TestSecpolGatewayDenyCounts(t *testing.T) {
	deny, open := scanFixture(t, gatewayDeny)
	if deny != 1 || open != 0 {
		t.Errorf("deny/open = %d/%d, want 1/0", deny, open)
	}
}

func TestSecpolUntargetedDenyCounts(t *testing.T) {
	deny, open := scanFixture(t, "kind: SecurityPolicy\nspec:\n  authorization:\n    defaultAction: Deny\n")
	if deny != 1 || open != 0 {
		t.Errorf("deny/open = %d/%d, want 1/0", deny, open)
	}
}

func TestSecpolRoutePolicyWithoutMergeIsOpen(t *testing.T) {
	deny, open := scanFixture(t, `kind: SecurityPolicy
spec:
  targetRef:
    kind: HTTPRoute
    name: app
  authorization:
    defaultAction: Allow
`)
	if deny != 0 || open != 1 {
		t.Errorf("deny/open = %d/%d, want 0/1", deny, open)
	}
}

func TestSecpolMergeTypeIsValueChecked(t *testing.T) {
	// The CRD has no enum for mergeType — its only validation is
	// `self != 'Replace'` — so `Merge`, `strategicmerge`, or "" pass
	// admission and merge NOTHING. Only StrategicMerge and JSONMerge count.
	base := `kind: SecurityPolicy
spec:
  targetRef:
    kind: GRPCRoute
    name: app
  mergeType: %s
  authorization:
    defaultAction: Allow
`
	for mt, wantOpen := range map[string]int{
		"StrategicMerge": 0, "JSONMerge": 0,
		"Merge": 1, "strategicmerge": 1, `""`: 1,
	} {
		content := "kind: SecurityPolicy\nspec:\n  targetRef:\n    kind: GRPCRoute\n    name: app\n  mergeType: " + mt + "\n  authorization:\n    defaultAction: Allow\n"
		_, open := scanFixture(t, content)
		if open != wantOpen {
			t.Errorf("mergeType %s: open = %d, want %d", mt, open, wantOpen)
		}
	}
	_ = base
}

func TestSecpolRoutePolicyWithOwnDenyNotCounted(t *testing.T) {
	// A route policy that re-denies replaces a deny-by-default with a
	// deny-by-default — equivalent in POSTURE, so not counted.
	deny, open := scanFixture(t, `kind: SecurityPolicy
spec:
  targetRef:
    kind: TCPRoute
    name: app
  authorization:
    defaultAction: Deny
    rules:
      - action: Allow
        principal:
          clientCIDRs: ["192.168.0.0/16"]
`)
	if deny != 0 || open != 0 {
		t.Errorf("deny/open = %d/%d, want 0/0 (routed deny counts as neither)", deny, open)
	}
}

func TestSecpolAllowAllCancelsDeny(t *testing.T) {
	// defaultAction: Deny + allow-all rule = the declaration is cosmetic; the
	// same predicate serves BOTH counts.
	gatewayAllowAll := `kind: SecurityPolicy
spec:
  targetRefs:
    - kind: Gateway
      name: g
  authorization:
    defaultAction: Deny
    rules:
      - action: Allow
        principal:
          clientCIDRs: ["0.0.0.0/0"]
`
	deny, _ := scanFixture(t, gatewayAllowAll)
	if deny != 0 {
		t.Errorf("an allow-all Deny is no carve-out, deny = %d", deny)
	}
	routeAllowAllDeny := `kind: SecurityPolicy
spec:
  targetRef:
    kind: HTTPRoute
    name: app
  authorization:
    defaultAction: Deny
    rules:
      - action: Allow
        principal:
          clientCIDRs: ["10.0.0.0/0"]
`
	// A /0 prefix with a non-zero address is STILL every IPv4 address — the
	// prefix is matched, not the literal 0.0.0.0/0.
	_, open := scanFixture(t, routeAllowAllDeny)
	if open != 1 {
		t.Errorf("a route Deny that allow-alls is open, open = %d", open)
	}
}

func TestSecpolAllowAllPredicateIsNarrow(t *testing.T) {
	cases := []struct {
		name string
		rule string
		deny int // for a Gateway-targeted Deny carrying this rule
	}{
		// operation scopes the rule to part of the surface → not allow-all.
		{"operation-scoped rule", `      - action: Allow
        operation:
          methods: [GET]
        principal:
          clientCIDRs: ["0.0.0.0/0"]
`, 1},
		// jwt beside the CIDR is a real guard, ANDed.
		{"jwt principal", `      - action: Allow
        principal:
          clientCIDRs: ["0.0.0.0/0"]
          jwt:
            provider: p
`, 1},
		// ::/0 counts (prefix match).
		{"ipv6 zero prefix", `      - action: Allow
        principal:
          clientCIDRs: ["::/0"]
`, 0},
		// A narrow CIDR is a real allowlist.
		{"narrow cidr", `      - action: Allow
        principal:
          clientCIDRs: ["10.0.0.0/8"]
`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := `kind: SecurityPolicy
spec:
  targetRefs:
    - kind: Gateway
      name: g
  authorization:
    defaultAction: Deny
    rules:
` + tc.rule
			deny, _ := scanFixture(t, content)
			if deny != tc.deny {
				t.Errorf("deny = %d, want %d", deny, tc.deny)
			}
		})
	}
}

func TestSecpolTLSRouteNotRouted(t *testing.T) {
	// TLSRoute/UDPRoute are excluded ON PURPOSE — a SecurityPolicy cannot
	// produce the replace bug on either (no attachment, no merge hierarchy).
	deny, open := scanFixture(t, `kind: SecurityPolicy
spec:
  targetRef:
    kind: TLSRoute
    name: app
  authorization:
    defaultAction: Allow
`)
	if deny != 0 || open != 0 {
		t.Errorf("deny/open = %d/%d, want 0/0", deny, open)
	}
}

func TestSecpolNonPolicyDocsIgnored(t *testing.T) {
	// A ConfigMap quoting the strings must not count (the per-object reader
	// replaced the old file-global grep exactly because of this).
	deny, open := scanFixture(t, "kind: ConfigMap\ndata:\n  x: |\n    defaultAction: Deny\n")
	if deny != 0 || open != 0 {
		t.Errorf("deny/open = %d/%d, want 0/0", deny, open)
	}
}

func TestSecpolUnparsableFileCountsNothing(t *testing.T) {
	deny, open := scanFixture(t, gatewayDeny+"---\n[broken\n")
	// Streaming: the document BEFORE the syntax error still counts (yq
	// prints per document); the broken tail contributes nothing.
	if deny != 1 || open != 0 {
		t.Errorf("deny/open = %d/%d, want 1/0", deny, open)
	}
}

func TestEncryptionEncryptsSecretsMultiDoc(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.yaml")
	writeFileT(t, f, "kind: ConfigMap\n---\nkind: EncryptionConfiguration\nresources:\n  - resources: [secrets]\n    providers:\n      - kmsv2:\n          name: k\n")
	if !encryptionEncryptsSecrets([]string{f}) {
		t.Error("kmsv2 writer across a multi-doc stream must prove encryption")
	}
}
