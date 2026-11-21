package secrets

import "github.com/kernpilot/lok8s/kustomize/pkg/storeguard"

// The .gitignore of a store, the name checks of a plaintext file and its
// write live in storeguard: the Secret generator of lo build writes its
// cache into the same store through the same checks. The names below keep
// the callers of this package short (bash twin: the same functions in
// libs/secrets).

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

// planGitignore is storeguard.PlanGitignore (bash: secrets::_gitignore_plan).
func planGitignore(raw []byte, rel string) (want []byte, problems []string) {
	return storeguard.PlanGitignore(raw, rel)
}

// reservedPlaintextName is storeguard.ReservedName (bash:
// secrets::_enc_reserved).
func reservedPlaintextName(base string) bool { return storeguard.ReservedName(base) }

// setPartProblem is storeguard.PartProblem (bash: secrets::_set_part_ok).
func setPartProblem(s string) string { return storeguard.PartProblem(s) }

// setDestination is storeguard.Destination (bash: secrets::_set_destination).
func setDestination(dir, base string) (string, bool) { return storeguard.Destination(dir, base) }

// printable is storeguard.Printable: each byte outside the printable ASCII
// range as ?.
func printable(s string) string { return storeguard.Printable(s) }
