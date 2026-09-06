package secrets

// Live-drift check (bash: secrets::_live_drift).
//
// Warn when the LIVE cluster Secret still holds a different value than the
// one just written to the store.
//
// `lo secrets set` updates the CACHE only — the running workload keeps its
// old value until `lo build` + apply. The gap itself is by design; the
// SILENCE is the bug. An operator who sets a value and reads back
// `Set app/default/KEY` has no reason to suspect the cluster disagrees — that
// assumption made a real drift incident expensive to find (2026-06-15).
//
// Advisory ONLY — every failure path returns without error. No kubectl, no
// current context, an unreachable cluster, an absent Secret or an absent key
// are all ordinary (the value may simply not be deployed yet) and must never
// fail a write that has already succeeded. `--request-timeout` is what keeps
// an unreachable cluster from hanging the CLI instead of just declining to
// answer.
//
// The comparison is base64-to-base64; the live side is never decoded. That
// keeps binary values out of intermediate strings, and it is what makes a
// trailing-newline difference VISIBLE: decoding the live value (the way a
// bash command substitution would) would strip its trailing newline, match it
// against a store value that never has one, and report "no drift" for a
// Secret that `lo build` + apply would genuinely rewrite. A false negative
// here is the expensive direction — it is the original silent trap wearing a
// green tick.
//
// The context is NAMED in the warning deliberately. A claim about "live" that
// silently refers to whichever cluster kubectl happens to point at is worse
// than no claim: it reads as authoritative while being about somewhere else.
//
// kubectl stays an external exec (it is not worth a client-go dependency for
// an advisory check), run with the KUBECONFIG the bash entrypoint would have
// exported.

import (
	"encoding/base64"
	"os"
	"os/exec"
	"strings"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

func (c *Context) liveDrift(name, namespace, key, value string) {
	kubectl, ok := execx.Look(c.Paths, "kubectl")
	if !ok {
		return
	}

	ctx, err := c.kubectlOutput(kubectl, "config", "current-context")
	if err != nil {
		return
	}
	ctx = trimTrailingNewlines(ctx)
	if ctx == "" {
		return
	}

	// Bracket form: a key like `tls.crt` reads as a nested path in the bare
	// `{.data.tls.crt}` form and would silently return empty — i.e. "no
	// drift".
	escaped := strings.ReplaceAll(key, ".", `\.`)
	live, err := c.kubectlOutput(kubectl, "--request-timeout=5s", "get", "secret", name,
		"--namespace", namespace,
		"-o", "jsonpath={.data['"+escaped+"']}")
	if err != nil {
		return
	}
	if live == "" {
		ui.Debug("no live %s/%s key %s on %s — nothing to compare", namespace, name, key, ctx)
		return
	}

	want := base64.StdEncoding.EncodeToString([]byte(value))

	if live == want {
		ui.Debug("live %s/%s key %s on %s already matches the store", namespace, name, key, ctx)
		return
	}

	ui.Warnf(c.ErrOut, "live Secret %s/%s key %s on context '%s' still holds the PREVIOUS value — the store is updated, the workload is NOT; run 'lo build' + apply (GitOps planes: commit and let Flux reconcile)", namespace, name, key, ctx)
}

// kubectlOutput runs kubectl with stderr discarded (bash: 2>/dev/null) and
// KUBECONFIG pinned to the entrypoint-derived path, mirroring the bash
// `export KUBECONFIG=…` that precedes every subcommand.
func (c *Context) kubectlOutput(kubectl string, args ...string) (string, error) {
	cmd := exec.Command(kubectl, args...)
	if c.Kubeconfig != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+c.Kubeconfig)
	}
	out, err := cmd.Output()
	return string(out), err
}
