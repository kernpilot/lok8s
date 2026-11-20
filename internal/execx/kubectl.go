package execx

import "path/filepath"

// KubeRCOff is the Env entry that turns off the kubectl user defaults file
// (kuberc) for one kubectl child. A kuberc can set a flag default for any
// kubectl command, for example --show-secrets on a diff (kubectl v1.36 and
// later), which prints the data of a Secret in the clear. lo sets it on
// each kubectl child that writes from a file or stdin (apply, diff,
// replace, patch), so these calls behave the same for each user.
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

// KubectlEnv is the Env of a child that lo starts through a generic
// wrapper (a function that takes the tool name and its arguments from its
// caller): the KubeRCOff entry for each kubectl child, whatever its
// arguments (a read too), and nil for any other tool. lo does not guess
// the subcommand from the arguments: a global flag with a separate value
// (--server <url>) before the subcommand would make the guess miss. A
// caller that runs one known kubectl write sets Env: []string{KubeRCOff}
// on its Cmd itself.
func KubectlEnv(name string) []string {
	if filepath.Base(name) == "kubectl" {
		return []string{KubeRCOff}
	}
	return nil
}
