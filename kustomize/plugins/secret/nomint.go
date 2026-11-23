package secret

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kernpilot/lok8s/kustomize/pkg/cache"
)

// NoMintError is the failure of a no-mint run (NoMintEnv) that would write
// the store or CAROOT: each entry it would write, by name only, and the
// error that stopped the run, if one did.
type NoMintError struct {
	Refused []cache.Refusal
	// Cause is the error that stopped the run after the first refusal
	// (for example a secretRef to an entry that the store lacks), or nil.
	Cause error
}

func (e *NoMintError) Error() string {
	var b strings.Builder
	unit := "values"
	if len(e.Refused) == 1 {
		unit = "value"
	}
	fmt.Fprintf(&b, "%s is on, so the plugin writes nothing, but this run must write %d %s:", NoMintEnv, len(e.Refused), unit)
	for _, r := range e.Refused {
		switch {
		case r.CARoot != "":
			fmt.Fprintf(&b, "\n  - the CA in CAROOT %s: CAROOT has no CA, so the run would make one", r.CARoot)
		case r.Generate:
			fmt.Fprintf(&b, "\n  - namespace %q, Secret %q, key %q: the store has no value, so the run would generate one", r.Namespace, r.Secret, r.Key)
		default:
			fmt.Fprintf(&b, "\n  - namespace %q, Secret %q, key %q: the run would write a value that the store does not hold", r.Namespace, r.Secret, r.Key)
		}
	}
	if e.Cause != nil {
		fmt.Fprintf(&b, "\nThe run also stopped with this error: %v", e.Cause)
	}
	fmt.Fprintf(&b, "\nPut the values in the store (lo secrets set, or lo secrets decrypt), then build again. Or build without %s.", NoMintEnv)
	return b.String()
}

// Unwrap returns the error that stopped the run, if one did.
func (e *NoMintError) Unwrap() error { return e.Cause }

// noMintErr returns the failure of a no-mint run that recorded a refusal,
// with cause as the error that stopped the run (nil when none did), else
// nil. Outside a no-mint run it is nil.
func (p *Plugin) noMintErr(cause error) error {
	refused := p.noMintRun.Refused()
	if len(refused) == 0 {
		return nil
	}
	if _, ok := errors.AsType[*NoMintError](cause); ok {
		return cause
	}
	return &NoMintError{Refused: refused, Cause: cause}
}
