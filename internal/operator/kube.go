package operator

// kube.go — the kubectl calls shared by the hooks, each with the exact argv
// and stream routing of its bash line.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kernpilot/lok8s/internal/execx"
)

// kube runs kubectl through the Runner seam with the hook's streams.
type kube struct {
	runner execx.Runner
	stdout io.Writer
	stderr io.Writer
}

func (k *kube) out() io.Writer {
	if k.stdout != nil {
		return k.stdout
	}
	return os.Stdout
}

func (k *kube) errOut() io.Writer {
	if k.stderr != nil {
		return k.stderr
	}
	return os.Stderr
}

// run is `kubectl <args>` with explicit streams (nil = the hook's own).
func (k *kube) run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	if stdout == nil {
		stdout = k.out()
	}
	if stderr == nil {
		stderr = k.errOut()
	}
	return k.runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: args, Stdin: stdin, Stdout: stdout, Stderr: stderr})
}

// capture is `$(kubectl <args> 2>/dev/null)`: stdout captured, stderr
// dropped.
func (k *kube) capture(ctx context.Context, args ...string) (string, error) {
	var out strings.Builder
	err := k.run(ctx, nil, &out, io.Discard, args...)
	return out.String(), err
}

// patchStatus is runtime.sh's hook::patch_status: merge-patch the status
// subresource; a failure is logged (kubectl's own output rides the hook's
// stdout — the bash `2>&1` inside an `if !`), never masked.
//
//	kubectl patch <kind> <name> -n <ns> --type merge --subresource status -p <patch>
func (k *kube) patchStatus(ctx context.Context, kind, name, namespace, patch string) {
	if err := k.run(ctx, nil, k.out(), k.out(), "patch", kind, name, "-n", namespace,
		"--type", "merge", "--subresource", "status", "-p", patch); err != nil {
		fmt.Fprintf(k.errOut(), "warn: failed to patch %s %s/%s status\n", kind, namespace, name)
	}
}

// patchStatusQuiet is the capi hooks' inline form: the same argv, stderr
// dropped, failure ignored (`2>/dev/null || true`).
func (k *kube) patchStatusQuiet(ctx context.Context, kind, name, namespace, patch string) error {
	return k.run(ctx, nil, nil, io.Discard, "patch", kind, name, "-n", namespace,
		"--type", "merge", "--subresource", "status", "-p", patch)
}

// ensureFinalizer adds finalizer to the CR unless finalizers already carries
// it (lo_hook::ensure_finalizer / capi_hook::ensure_finalizer): a JSON-patch
// append (stderr dropped — it fails when the finalizers array does not exist
// yet), then the create-the-array form, then the warn line.
func (k *kube) ensureFinalizer(ctx context.Context, kind, kindLabel, name, namespace string, finalizers any, finalizer string) {
	if contains(finalizers, finalizer) {
		return
	}
	appendPatch := `[{"op":"add","path":"/metadata/finalizers/-","value":"` + finalizer + `"}]`
	if err := k.run(ctx, nil, nil, io.Discard, "patch", kind, name, "-n", namespace, "--type", "json", "-p", appendPatch); err == nil {
		return
	}
	createPatch := `[{"op":"add","path":"/metadata/finalizers","value":["` + finalizer + `"]}]`
	if err := k.run(ctx, nil, nil, nil, "patch", kind, name, "-n", namespace, "--type", "json", "-p", createPatch); err == nil {
		return
	}
	fmt.Fprintf(k.errOut(), "warn: failed to add finalizer to %s %s/%s\n", kindLabel, namespace, name)
}

// removeFinalizer drops finalizer from the CR (lo_hook::remove_finalizer /
// capi_hook::remove_finalizer). The bash pipeline
//
//	remaining=$(kubectl get … -o jsonpath='{.metadata.finalizers}' 2>/dev/null |
//	  jq -c --arg f F 'map(select(. != $f))' 2>/dev/null || echo '[]')
//
// under pipefail resolves to: kubectl failed OR jq could not parse →
// "[]"; kubectl succeeded with EMPTY output (no finalizers field) → jq
// emits nothing → remaining is "" and the merge patch below is malformed
// (kubectl rejects it → the warn line). Reproduced as-is: bash wins.
func (k *kube) removeFinalizer(ctx context.Context, kind, kindLabel, name, namespace, finalizer string) {
	out, err := k.capture(ctx, "get", kind, name, "-n", namespace, "-o", "jsonpath={.metadata.finalizers}")
	remaining := "[]"
	if err == nil {
		switch parsed, perr := decode([]byte(out)); {
		case strings.TrimSpace(out) == "":
			remaining = ""
		case perr != nil:
			remaining = "[]"
		default:
			arr, ok := parsed.([]any)
			if !ok {
				// jq: map() on a non-array errors → "[]".
				remaining = "[]"
				break
			}
			kept := []any{}
			for _, item := range arr {
				if s, ok := item.(string); ok && s == finalizer {
					continue
				}
				kept = append(kept, item)
			}
			remaining = compact(kept)
		}
	}
	patch := `{"metadata":{"finalizers":` + remaining + `}}`
	if err := k.run(ctx, nil, nil, nil, "patch", kind, name, "-n", namespace, "--type", "merge", "-p", patch); err != nil {
		fmt.Fprintf(k.errOut(), "warn: failed to remove finalizer from %s %s/%s\n", kindLabel, namespace, name)
	}
}

// listAll is `kubectl get <kind> -A -o json 2>/dev/null | jq -c '.items //
// []'`: the CR list for the schedule/synchronization re-list. A failed
// kubectl leaves jq with empty input (no output, rc 0) and the bash loop
// count empty → zero iterations; a list without .items → []. Either way:
// no items, no error.
func (k *kube) listAll(ctx context.Context, kind string) []any {
	out, err := k.capture(ctx, "get", kind, "-A", "-o", "json")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	doc, perr := decode([]byte(out))
	if perr != nil {
		return nil
	}
	items, _ := alt(get(doc, "items"), []any{}).([]any)
	return items
}
