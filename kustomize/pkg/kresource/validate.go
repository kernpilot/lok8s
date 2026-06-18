package kresource

import (
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
)

// ValidateSecretKeys checks that the given data map contains all keys
// required by the named Secret type. Returns nil if the type is unknown
// or has no required keys, or if all required keys are present.
//
// Returns a single *errs.Error listing all missing keys (sorted) on
// failure. The user-facing message names the type and missing keys so
// the fix is obvious.
func ValidateSecretKeys(secretType string, dataKeys map[string]struct{}) error {
	required := RequiredKeys(secretType)
	if len(required) == 0 {
		return nil
	}
	var missing []string
	for _, k := range required {
		if _, ok := dataKeys[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return errs.Newf("type %s requires keys %v", secretType, missing)
}
