package secret

import (
	"strings"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
)

// Env knobs that shape how the plugin behaves at the Run boundary. Both
// are read from the process environment (via the env func the runner
// threads through) so a caller — chiefly the `lo build` pipeline — can
// steer a `kustomize build` invocation without editing any Secret CRD.
const (
	// DisableEnv, when truthy, is the store-free OFF switch: the plugin
	// emits NOTHING and never reads $PATH_SECRETS, never touches or mints
	// the cache, and never runs a generator. It is checked at the very top
	// of Run, before Decode/Build, so a render can succeed with no secrets
	// store and no key present at all. kustomize accepts a generator that
	// yields zero resources.
	//
	// Truthy = "1" or "true" (case-insensitive). Any other value is off.
	DisableEnv = "LOK8S_SECRETS_DISABLE"

	// OutputEnv selects the plugin's output mode. The only recognized value
	// is "none": run the FULL pipeline (Decode + Build + generators, so
	// store reads/mints and validation still happen — side effects intact)
	// but SUPPRESS the final emit write, so zero resources are rendered.
	// Use case: prime/validate the cache without rendering. Distinct from
	// DisableEnv, which does not touch the store at all.
	//
	// An empty value is normal (emit). Any other non-empty value is
	// rejected (fail closed) so a typo can't silently emit secrets when the
	// caller expected suppression.
	OutputEnv = "LOK8S_SECRETS_OUTPUT"

	// OutputNone is the sole recognized OutputEnv value.
	OutputNone = "none"

	// KeysOnlyEnv is an INTERNAL contract between lo and this plugin, not
	// a user setting: lo sets it to 1 only for `lo build --keys-only` and
	// to 0 for every other call, and nobody exports it. When it is on, and
	// DisableEnv is on too, the plugin decodes the spec and emits the
	// Secret with every key that the spec names and an empty value for
	// each, plus the KeysOnlyAnnotation. It reads nothing but the spec: no
	// store, no file, no environment variable, no script, no CA.
	//
	// On = "1" or "true", off = unset, "", "0" or "false" (case-insensitive,
	// trimmed). Any other value is an error (a value of spaces only too),
	// and on without DisableEnv on is an error: a typo or a missing
	// DisableEnv never falls through to a run that reads the store.
	KeysOnlyEnv = "LOK8S_INTERNAL_SECRETS_KEYS_ONLY"

	// KeysOnlyAnnotation marks each Secret of a keys-only run. Its values
	// are empty, so it must never be applied to a cluster.
	KeysOnlyAnnotation = "secrets.lok8s.dev/keys-only"
)

// keysOnly reports whether KeysOnlyEnv is on. It fails closed: a value
// that is not one of the on or off values is an error, a value of spaces
// only too (like OutputEnv).
func keysOnly(env func(string) (string, bool)) (bool, error) {
	v, ok := env(KeysOnlyEnv)
	if !ok || v == "" {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	default:
		return false, errs.New(KeysOnlyEnv + `: unknown value ` + quote(v) + ` (want "1", "0" or unset)`)
	}
}

// disabled reports whether the store-free OFF switch is set. Truthy values
// are "1" and "true" (case-insensitive); everything else (incl. unset and
// "0"/"false") is off.
func disabled(env func(string) (string, bool)) bool {
	v, ok := env(DisableEnv)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true":
		return true
	default:
		return false
	}
}

// suppressOutput reports whether the emit write must be suppressed
// (LOK8S_SECRETS_OUTPUT=none). It fails closed: any value that is PRESENT and
// non-empty but not "none" is rejected — including a whitespace-only value
// (which trims to ""), so a typo can never silently fall through to a normal
// emit. Only truly unset OR an explicitly-empty value ("") means normal emit.
func suppressOutput(env func(string) (string, bool)) (bool, error) {
	v, ok := env(OutputEnv)
	if !ok || v == "" {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case OutputNone:
		return true, nil
	default:
		return false, errs.New(OutputEnv + `: unknown value ` + quote(v) + ` (want "none" or unset)`)
	}
}

// quote wraps s in double quotes for error messages without pulling in fmt.
func quote(s string) string { return `"` + s + `"` }

// Capabilities is what `Secret --capabilities` prints, one word per line:
// "dry-run" means the plugin honours LOK8S_BOOTSTRAP_DRYRUN (it reads the
// store, generates a missing value in memory, writes nothing and reports
// what it generated to LOK8S_BOOTSTRAP_DRYRUN_REPORT).
const Capabilities = "dry-run"
