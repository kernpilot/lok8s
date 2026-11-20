package cli

// cmd_doctor_ssogate.go is the SecurityPolicy CRD check of `lo doctor` for
// the sso-gate addon (issue #141). Bash twin: doctor::_sso_check in
// .lok8s/libs/doctor. The piped lines are the same in both.
//
// The SecurityPolicy of sso-gate sets spec.mergeType, so that the policy
// adds to the guards of the Gateway's own policy and does not replace them.
// envoy-gateway declares that field in its SecurityPolicy CRD from v1.8.0.
// On an older CRD, a client-side apply drops the field without an error,
// and the route then has the SSO policy only: the Gateway's guards (an IP
// allowlist, a deny-by-default authorization) no longer apply to it.
//
// The check runs only when spec.bootstrap has sso-gate as a bare entry name
// (read with the entry parser of `lo addons --detail`, as the RBAC migration
// check reads its addons) and the domain has a kubeconfig (the file that the
// bash entrypoint exports as KUBECONFIG). It runs one kubectl command:
// `kubectl get customresourcedefinitions.apiextensions.k8s.io
// securitypolicies.gateway.envoyproxy.io --ignore-not-found -o json`, with
// the bound, the process group and the fixed reasons of the RBAC migration
// check (rbacGet). The full resource name needs no short name (`crd`) from
// the API discovery of the cluster. kubectl sends GET requests only: the
// CRD, and the API discovery when its cache has no answer. kubectl's stderr
// is never printed.
//
// The check selects the version v1alpha1 by its name, never by its position
// in spec.versions (the order of that list is not a contract), and looks
// for schema.openAPIV3Schema.properties.spec.properties.mergeType in it.
// Two flags of the versions also count (the apiserver of
// k8s.io/apiextensions-apiserver):
//   - served: the apiserver answers a request to a version that is not
//     served as for no version. An apply of sso-gate then fails with an
//     error (no match for the kind), and drops nothing in silence: the
//     line is the one of a CRD without v1alpha1 (not checked).
//   - storage: exactly one version is the storage version. The apiserver
//     converts each object to that version before it stores it, and then
//     prunes the object with the schema of that version. A storage version
//     other than v1alpha1 that does not declare the field drops it in
//     silence, also when v1alpha1 declares it: that is a ✗.
//
// The lines: ✓ when v1alpha1 and the storage version declare the field; ✗
// when v1alpha1 does not (upgrade envoy-gateway first) or the storage
// version does not (reinstall the CRDs of envoy-gateway first); ℹ (skipped)
// without a kubeconfig or a CRD; ! (not checked) when kubectl is not there,
// the read fails, or the CRD has no served v1alpha1, with the fixed reason.
// Every line is advisory: the check never changes the doctor exit code.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

const (
	ssoAddon       = "sso-gate"
	ssoCRDResource = "customresourcedefinitions.apiextensions.k8s.io"
	ssoCRD         = "securitypolicies.gateway.envoyproxy.io"
	ssoVersion     = "v1alpha1"
)

// The lines of the check.
const (
	ssoDeclared         = "sso-gate: the SecurityPolicy CRD declares spec.mergeType"
	ssoTooOld           = "sso-gate: the SecurityPolicy CRD has no spec.mergeType, so envoy-gateway is older than v1.8.0: upgrade it first, then reconcile sso-gate"
	ssoDropped          = "sso-gate: the storage version of the SecurityPolicy CRD has no spec.mergeType, so the apiserver drops the field when it stores the policy: reinstall the CRDs of envoy-gateway first, then reconcile sso-gate"
	ssoSkipped          = "sso-gate SecurityPolicy CRD check skipped: "
	ssoNotChecked       = "sso-gate SecurityPolicy CRD not checked: "
	ssoReasonNoCRD      = "the cluster has no SecurityPolicy CRD (envoy-gateway is not installed)"
	ssoReasonNoVersion  = "the SecurityPolicy CRD has no version " + ssoVersion
	ssoReasonNotServed  = "the SecurityPolicy CRD does not serve the version " + ssoVersion
	ssoReasonForbidden  = "the kubeconfig user cannot read the SecurityPolicy CRD (forbidden or unauthorized)"
	ssoReasonInvalidCRD = "kubectl failed or did not return a valid CRD"
)

// The path of the field in the schema of a version.
var ssoFieldPath = []string{"schema", "openAPIV3Schema", "properties", "spec", "properties", "mergeType"}

// ssoState is what the read of the CRD gives (bash: doctor::_sso_state).
type ssoState int

const (
	ssoInvalid       ssoState = iota // not a CRD of the shape that the check reads
	ssoNoCRD                         // no CRD of that name
	ssoNoVersion                     // the CRD has no version v1alpha1
	ssoNotServed                     // v1alpha1 is not served
	ssoAbsent                        // v1alpha1 does not declare the field
	ssoStorageAbsent                 // the storage version (not v1alpha1) does not declare it
	ssoPresent                       // v1alpha1 and the storage version declare the field
)

// doctorSSOGateMergeType prints the line of the check under the domain
// section. resolve reads spec.bootstrap (bootstrapEntries in production).
// Without a domain, with a deploy domain, without a cluster spec or with a
// spec that cannot be read, it prints nothing: the RBAC migration check
// above says it.
func doctorSSOGateMergeType(ctx context.Context, r execx.Runner, paths *config.Paths, d, path string, resolve addons.EntryResolver, out io.Writer) {
	if !domain.NameRe.MatchString(d) {
		return
	}
	spec := filepath.Join(paths.Clusters, d, "cluster.lok8s.yaml")
	if !fsutil.FileExists(spec) {
		return
	}
	kind, err := domain.SpecDriver(spec, "lo")
	if err != nil {
		return
	}
	// The addon counts only as a bare entry name (Builtin), never as a path
	// entry, as in the RBAC migration check.
	enabled := false
	for _, e := range resolve(spec, kind, d) {
		if e.Builtin && filepath.Base(e.Dir) == ssoAddon {
			enabled = true
		}
	}
	if !enabled {
		return
	}
	kubeconfig := build.AmbientKubeconfig(paths, d, "")
	if rbacHasControl(kubeconfig) {
		doctorInfo(out, ssoSkipped+"the kubeconfig path has a control character")
		return
	}
	if !fsutil.FileExists(kubeconfig) {
		doctorInfo(out, ssoSkipped+"no kubeconfig at "+config.RelTo(paths.Base, kubeconfig))
		return
	}
	kubectl, ok := toolchain.LookPath(path, "kubectl")
	if !ok {
		doctorWarn(out, ssoNotChecked+rbacReasonNoKubectl)
		return
	}
	data, reason := rbacGet(ctx, r, kubectl, kubeconfig, []string{"get", ssoCRDResource, ssoCRD, "--ignore-not-found"})
	switch reason {
	case "":
	case rbacReasonForbidden:
		reason = ssoReasonForbidden
	case rbacReasonOther:
		reason = ssoReasonInvalidCRD
	}
	if reason != "" {
		doctorWarn(out, ssoNotChecked+reason)
		return
	}
	switch ssoParse(data) {
	case ssoPresent:
		doctorOK(out, ssoDeclared)
	case ssoAbsent:
		doctorBad(out, ssoTooOld)
	case ssoStorageAbsent:
		doctorBad(out, ssoDropped)
	case ssoNoCRD:
		doctorInfo(out, ssoSkipped+ssoReasonNoCRD)
	case ssoNoVersion:
		doctorWarn(out, ssoNotChecked+ssoReasonNoVersion)
	case ssoNotServed:
		doctorWarn(out, ssoNotChecked+ssoReasonNotServed)
	default:
		doctorWarn(out, ssoNotChecked+ssoReasonInvalidCRD)
	}
}

// ssoParse reads the output of the CRD read (bash: doctor::_sso_state, the
// same rules). An output of JSON white space only, or a List with no items,
// is no CRD (--ignore-not-found prints nothing for a CRD that is not
// there). Else it must be one JSON object with a spec.versions list of
// objects, each with a string name (absent or null: empty) and the
// booleans served and storage (the apiserver always prints both). Exactly
// one element must be the storage version, and exactly one must have the
// name v1alpha1. A v1alpha1 that is not served is ssoNotServed. Then the
// field (ssoField) in v1alpha1, and, when v1alpha1 declares it and is not
// the storage version, in the storage version: there, a field that is
// absent is ssoStorageAbsent. The decode into float64 refuses a number out of
// its range and a byte-order mark, as the bash twin does.
func ssoParse(data []byte) ssoState {
	if len(bytes.Trim(data, " \t\r\n")) == 0 {
		return ssoNoCRD
	}
	var doc any
	if json.Unmarshal(data, &doc) != nil {
		return ssoInvalid
	}
	top, ok := doc.(map[string]any)
	if !ok {
		return ssoInvalid
	}
	if k, _ := top["kind"].(string); k == "List" {
		if items, ok := top["items"].([]any); ok && len(items) == 0 {
			return ssoNoCRD
		}
		return ssoInvalid
	}
	spec, ok := top["spec"].(map[string]any)
	if !ok {
		return ssoInvalid
	}
	versions, ok := spec["versions"].([]any)
	if !ok {
		return ssoInvalid
	}
	var version, stored map[string]any
	n, nStored := 0, 0
	for _, v := range versions {
		m, ok := v.(map[string]any)
		if !ok {
			return ssoInvalid
		}
		name, ok := rbacString(m["name"])
		if !ok {
			return ssoInvalid
		}
		if _, ok := m["served"].(bool); !ok {
			return ssoInvalid
		}
		storage, ok := m["storage"].(bool)
		if !ok {
			return ssoInvalid
		}
		if storage {
			stored = m
			nStored++
		}
		if name == ssoVersion {
			version = m
			n++
		}
	}
	switch {
	case nStored != 1:
		return ssoInvalid
	case n == 0:
		return ssoNoVersion
	case n > 1:
		return ssoInvalid
	}
	if !version["served"].(bool) {
		return ssoNotServed
	}
	state := ssoField(version)
	if state != ssoPresent || version["storage"].(bool) {
		return state
	}
	if state = ssoField(stored); state == ssoAbsent {
		return ssoStorageAbsent
	}
	return state
}

// ssoField reads the field in one version of the CRD: each step of
// ssoFieldPath that is there must be an object. A step that is absent or
// null makes the field absent (ssoAbsent), any other value makes the CRD
// invalid. A field that is an object is ssoPresent.
func ssoField(version map[string]any) ssoState {
	var v any = version
	for _, k := range ssoFieldPath {
		if v == nil {
			return ssoAbsent
		}
		m, ok := v.(map[string]any)
		if !ok {
			return ssoInvalid
		}
		v = m[k]
	}
	switch v.(type) {
	case nil:
		return ssoAbsent
	case map[string]any:
		return ssoPresent
	}
	return ssoInvalid
}
