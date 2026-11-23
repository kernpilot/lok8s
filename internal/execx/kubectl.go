package execx

import (
	"path/filepath"
	"strings"
)

// KubeRCOff is the Env entry that turns off the kubectl user defaults file
// (kuberc) for one kubectl child. A kuberc can set a flag default for any
// kubectl command: --dry-run=client makes a delete, a patch or a rollout
// restart report success and change nothing, --append-hash renames the
// Secret that a render makes, and --show-secrets on a diff (kubectl v1.36
// and later) prints the data of a Secret in the clear. lo sets it on each
// kubectl child that writes: a call that changes an object (apply, create,
// replace, patch, delete, annotate, label, taint, a rollout restart and the
// like), that runs a command in a pod or copies to it (exec, cp, attach,
// debug), that changes the kubeconfig (config set-context and the like), a
// diff, and a render (create --dry-run=client) whose output lo applies. So
// these calls behave the same for each user. A read keeps the kuberc of the
// user. The scan TestEveryKubectlWriteRunsWithKubeRCOff holds the list.
//
// kubectl v1.33 and later read KUBERC: the value "off" loads no kuberc
// file. An older kubectl has no kuberc and does not read the variable.
// lo does not use the --kuberc flag: a kubectl before v1.33 refuses an
// unknown flag.
//
// Only the child gets the variable: lo's own environment and its other
// children keep what the user set. Cmd.Env comes after the inherited
// environment, and os/exec keeps the last entry of a name, so a KUBERC of
// the user does not reach these calls. For the same reason KubeRCOff must
// be the last KUBERC entry of the Env (KubeRCIsOff). The bash tree puts the
// same assignment in front of each such kubectl call (KUBERC=off kubectl …).
const KubeRCOff = "KUBERC=off"

// KubeRCIsOff reports whether a child with this Env runs with the kubectl
// user defaults off: the last KUBERC entry of env is KubeRCOff. os/exec
// keeps the last entry of a name, so a KubeRCOff with another KUBERC entry
// after it does not count.
func KubeRCIsOff(env []string) bool {
	off := false
	for _, e := range env {
		if strings.HasPrefix(e, "KUBERC=") {
			off = e == KubeRCOff
		}
	}
	return off
}

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
