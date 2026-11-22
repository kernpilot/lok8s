package secrets

import "github.com/kernpilot/lok8s/kustomize/pkg/storeguard"

// The .gitignore of a store, the name checks of a plaintext file and its
// write live in storeguard: the Secret generator of lo build writes its
// cache into the same store through the same checks. The names below keep
// the callers of this package short (bash twin: the same functions in
// libs/secrets). The plan, the part check and the destination check of
// set have no name here: the tests call storeguard.PlanGitignore,
// storeguard.PartProblem and storeguard.Destination.

// The steps that a .gitignore check stops before. Each refusal of the check
// names the step of its command: an earlier step of the command can have
// written a file already.
const (
	stepFirstWrite     = "its first write"
	stepGitignoreWrite = "the .gitignore write"
	stepSopsConfig     = "the change of .sops.yaml"
	stepPlaintext      = storeguard.StepPlaintext
	stepEncWrites      = "the .enc writes"
	stepDecrypt        = "the first plaintext write"
)

// reservedPlaintextName is storeguard.ReservedName (bash:
// secrets::_enc_reserved).
func reservedPlaintextName(base string) bool { return storeguard.ReservedName(base) }

// printable is storeguard.Printable: each byte outside the printable ASCII
// range as ?.
func printable(s string) string { return storeguard.Printable(s) }
