package execx

import (
	"slices"
	"strings"
)

// KubeRCOff is the Env entry that turns off the kubectl user defaults file
// (kuberc) for one kubectl child. A kuberc can set a flag default for any
// kubectl command, for example --show-secrets on a diff (kubectl v1.36 and
// later), which prints the data of a Secret in the clear. lo sets it on
// each kubectl apply and kubectl diff that it runs, so these calls behave
// the same for each user.
//
// kubectl v1.33 and later read KUBERC: the value "off" loads no kuberc
// file. An older kubectl has no kuberc and does not read the variable.
// lo does not use the --kuberc flag: a kubectl before v1.33 refuses an
// unknown flag.
//
// Only the child gets the variable: lo's own environment and its other
// children keep what the user set. Cmd.Env comes after the inherited
// environment, and os/exec keeps the last entry of a name, so a KUBERC of
// the user does not reach these calls. The bash tree puts the same
// assignment in front of each such kubectl call (KUBERC=off kubectl …).
const KubeRCOff = "KUBERC=off"

// KubectlApplyOrDiff reports whether name and args start a kubectl apply
// or a kubectl diff. The subcommand is the first argument that is not a
// flag; the separate value of a global flag that lo puts before the
// subcommand (--kubeconfig <path>, -n/--namespace <ns>, --context <name>)
// is skipped.
func KubectlApplyOrDiff(name string, args []string) bool {
	if name != "kubectl" {
		return false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if slices.Contains(valueFlags, a) {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a == "apply" || a == "diff"
	}
	return false
}

// valueFlags are the global kubectl flags that lo passes with a separate
// value (KubectlApplyOrDiff skips the value).
var valueFlags = []string{"--kubeconfig", "-n", "--namespace", "--context"}

// KubectlEnv is the Env of a child that lo starts through a generic
// wrapper (a function that takes the tool name and its arguments): the
// KubeRCOff entry for a kubectl apply or diff, nil for every other call.
func KubectlEnv(name string, args []string) []string {
	if KubectlApplyOrDiff(name, args) {
		return []string{KubeRCOff}
	}
	return nil
}
