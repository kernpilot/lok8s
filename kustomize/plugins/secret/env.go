package secret

import (
	"strings"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
)

// Env knobs that shape how the plugin behaves at the Run boundary. They
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

	// NoMintEnv, when on, makes the store read-only for the run: the
	// plugin generates no value into the store and writes no file. It
	// reads the store through the read-only overlay of the dry run
	// (cache.NewNoMint). Each entry that the run would generate (the
	// store has no plaintext and no .enc twin) or write (an env: or
	// update: true value that the store does not hold), and a CA that it
	// would create in CAROOT, is recorded. After every generator of the
	// Secret ran, the run fails and names each one: namespace, Secret
	// name and key, never a value or a length. A bash: entry that the
	// store lacks does not run its command. An update: true command still
	// runs; only its value is checked. The command itself can write
	// anything, so the switch does not make such a command safe. With
	// nothing to record, the run emits the same Secret as a run without
	// the switch. kustomize stops at the first plugin that fails, so one
	// failed run names the entries of one Secret. The error text of a run
	// that also stopped on an error (NoMintError.Cause) is not redacted:
	// it is the text that a run without the switch prints, and a bash:
	// command's stderr can be in it.
	//
	// On = "1" or "true", the on values of DisableEnv (truthy). Off =
	// unset, "", "0" or "false". Any other value is an error before
	// anything is read (a value of spaces only too): a typo never turns
	// the switch off. This differs from DisableEnv, where any other value
	// is off: here an off typo would let the run generate values, which is
	// the failure that the switch exists to stop. It wins over a dry run
	// (LOK8S_BOOTSTRAP_DRYRUN): the run fails, it does not report.
	// DisableEnv wins over it: that run reads no store and generates
	// nothing, so there is nothing to refuse.
	//
	// It is no switch of the implementation (AGENTS.md: no environment
	// variable selects the implementation): with it on, a run emits what a
	// run without it emits, or fails, and writes nothing, so an inherited
	// value can only make a run fail closed.
	NoMintEnv = "LOK8S_SECRETS_NO_MINT"

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
	return ok && truthy(v)
}

// truthy reports whether v is an on value of DisableEnv and NoMintEnv:
// "1" or "true", case-insensitive, with the spaces around it trimmed.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true":
		return true
	default:
		return false
	}
}

// NoMintRequested reports whether env asks for the no-mint run: NoMintEnv
// holds a value that is not an off value (unset, "", "0" or "false"). An
// unknown value counts too: this plugin refuses it, but an older plugin
// that does not know the variable ignores it and generates values. lo
// checks the plugin's capabilities when this is true.
func NoMintRequested(env func(string) (string, bool)) bool {
	on, err := noMint(env)
	return on || err != nil
}

// noMint reports whether NoMintEnv is on. It fails closed: a value that is
// not an on value (truthy) and not an off value is an error, a value of
// spaces only too (like KeysOnlyEnv).
func noMint(env func(string) (string, bool)) (bool, error) {
	v, ok := env(NoMintEnv)
	if !ok || v == "" {
		return false, nil
	}
	if truthy(v) {
		return true, nil
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false":
		return false, nil
	default:
		return false, errs.New(NoMintEnv + `: unknown value ` + quote(v) + ` (want "1", "true", "0", "false" or unset)`)
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
// what it generated to LOK8S_BOOTSTRAP_DRYRUN_REPORT). "keys-only" means
// the plugin has the keys-only run (KeysOnlyEnv with DisableEnv): `lo build
// --keys-only` refuses a plugin without it in the exec pipeline. "no-mint"
// means the plugin honours NoMintEnv: an older plugin ignores the variable
// and generates, so a caller that sets it checks for this line first.
const Capabilities = "dry-run\nkeys-only\nno-mint"
