package secret

import (
	"io"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
)

// Run is the secret plugin's entrypoint, wrapping the generic
// plugin.RunEnv runner with the store-shaping env knobs
// (LOK8S_SECRETS_DISABLE / LOK8S_SECRETS_OUTPUT, and lo's internal
// LOK8S_INTERNAL_SECRETS_KEYS_ONLY). cmd/secret/main.go
// calls this instead of plugin.Run directly so the knobs are honored on
// every kustomize invocation.
//
// The env func is threaded through so tests can drive the knobs
// hermetically; production passes plugin.DefaultEnv (os.LookupEnv).
//
// Precedence: LOK8S_SECRETS_DISABLE wins over LOK8S_SECRETS_OUTPUT —
// disable means "do nothing at all", so the store is never consulted.
//
//   - DISABLE (truthy): emit nothing and short-circuit BEFORE the runner,
//     so Decode/Build/store access never happen. Returns success with an
//     empty stdout; kustomize accepts a generator that yields zero
//     resources. This is what makes a render store-free.
//   - DISABLE and LOK8S_INTERNAL_SECRETS_KEYS_ONLY (both on): the
//     keys-only run (keysonly.go). It reads the spec only and emits each
//     key with an empty value. lo sets it for `lo build --keys-only` only.
//   - KEYS_ONLY on without DISABLE on, or a KEYS_ONLY value that is not
//     known: an error before anything is read, never a run that reads
//     the store.
//   - OUTPUT=none: run the FULL pipeline (store reads/mints + validation
//     intact) but discard the emit write, so zero resources are rendered.
//   - otherwise: normal emit.
//
// A plugin older than KEYS_ONLY sees DISABLE only and emits nothing.
func Run(argv []string, stdin io.Reader, stdout io.Writer, env func(string) (string, bool)) error {
	keys, err := keysOnly(env)
	if err != nil {
		return err
	}
	// Store-free OFF switch: checked first (wins over OUTPUT), before any
	// store access. Emit nothing (or, keys-only, the keys), succeed.
	if disabled(env) {
		if keys {
			return runKeysOnly(argv, stdin, stdout)
		}
		return nil
	}
	if keys {
		return errs.New(KeysOnlyEnv + " is on, but " + DisableEnv + " is not 1: a keys-only run never reads the store")
	}

	// Run-but-suppress: the pipeline runs (side effects intact) but its
	// output is discarded. A bad LOK8S_SECRETS_OUTPUT value fails closed.
	suppress, err := suppressOutput(env)
	if err != nil {
		return err
	}
	out := stdout
	if suppress {
		out = io.Discard
	}

	return plugin.RunEnv(argv, stdin, out, env, New())
}
