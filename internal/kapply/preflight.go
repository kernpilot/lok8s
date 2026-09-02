// preflight.go — kapply::preflight: sweep the MANIFEST's objects for
// stuck-Terminating state BEFORE an apply that does not go through
// Applier.Apply — Tilt's k8s_yaml path retries a terminating object until
// its upsert timeout and then fails the whole build. Objects terminating
// LONGER than --age (default 30s, or KAPPLY_PREFLIGHT_AGE) get their
// finalizers cleared so the delete completes and the follow-up apply
// recreates them; younger deletions are presumed to be legitimately
// draining. Namespaces finalize via the /finalize subresource.
//
// NO prompts here — the CALLER is the gate. Best-effort: per-object failures
// warn; always returns nil so a preflight can never brick the deploy it
// protects.
package kapply

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/execx"
)

var crdsPolicyRe = regexp.MustCompile(`^(drain|skip|force)$`)

type preflightObj struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Namespace         string   `json:"namespace"`
		Name              string   `json:"name"`
		DeletionTimestamp string   `json:"deletionTimestamp"`
		Finalizers        []string `json:"finalizers"`
	} `json:"metadata"`
}

// decodePreflightObjs handles jq's `(.items // [.])[]?` over one JSON doc:
// a List's items, or the single object itself.
func decodePreflightObjs(raw string) []preflightObj {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var list struct {
		Items *[]preflightObj `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err == nil && list.Items != nil {
		return *list.Items
	}
	var one preflightObj
	if err := json.Unmarshal([]byte(raw), &one); err == nil && one.Kind != "" {
		return []preflightObj{one}
	}
	return nil
}

// Preflight sweeps the manifest for stuck-Terminating objects (bash:
// kapply::preflight). args carries the bash CLI surface — --age <s>,
// --crds drain|skip|force, --crd-allow <csv> — plus passthrough kubectl
// flags. Always returns nil.
func (a *Applier) Preflight(ctx context.Context, manifest string, args ...string) error {
	age := envInt("KAPPLY_PREFLIGHT_AGE", 30)
	crds := envDefault("KAPPLY_PREFLIGHT_CRDS", "drain")
	crdAllow := ""
	var kubectlFlags []string
	for i := 0; i < len(args); i++ {
		// Tolerate a value-less flag (keep the default): swallowing a
		// following flag as the value would misfile its own argument.
		takeValue := func() (string, bool) {
			if i+1 < len(args) && args[i+1] != "" && !strings.HasPrefix(args[i+1], "-") {
				i++
				return args[i], true
			}
			return "", false
		}
		switch args[i] {
		case "--age":
			if v, ok := takeValue(); ok {
				if n, err := strconv.Atoi(v); err == nil {
					age = n
				} else {
					age = -1 // non-numeric → the fallback below
				}
			}
		case "--crds":
			if v, ok := takeValue(); ok {
				crds = v
			}
		case "--crd-allow":
			if v, ok := takeValue(); ok {
				crdAllow = v
			}
		default:
			kubectlFlags = append(kubectlFlags, args[i])
		}
	}
	// The age gate is a SAFETY threshold — a non-numeric value would
	// force-clear everything terminating at all. Fall back to the default.
	if age < 0 {
		age = 30
	}
	// Unknown CRD policy falls back to drain — the safe middle.
	if !crdsPolicyRe.MatchString(crds) {
		crds = "drain"
	}

	// ONE batched GET for every manifest object. Unknown kinds (a CRD not
	// yet installed) make kubectl error without stopping the sweep —
	// DELIBERATELY silenced: the apply that follows surfaces any real
	// problem anyway.
	getArgs := append(append([]string{}, kubectlFlags...),
		"get", "-f", "-", "-o", "json", "--ignore-not-found")
	var liveBuf strings.Builder
	_ = a.Runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: getArgs,
		Stdin: strings.NewReader(manifest), Stdout: &liveBuf, Stderr: io.Discard})

	// A namespace REFERENCED by the manifest but not declared in it still
	// blocks every create inside it while terminating — fetch those too.
	refSeen := map[string]bool{}
	var referenced []string
	for _, d := range parseManifestDocs(manifest) {
		if d.namespace != "" && !refSeen[d.namespace] {
			refSeen[d.namespace] = true
			referenced = append(referenced, d.namespace)
		}
	}
	sort.Strings(referenced)
	var nsBuf strings.Builder
	if len(referenced) > 0 {
		nsArgs := append(append([]string{}, kubectlFlags...), "get", "namespace")
		nsArgs = append(nsArgs, referenced...)
		nsArgs = append(nsArgs, "-o", "json", "--ignore-not-found")
		_ = a.Runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: nsArgs,
			Stdout: &nsBuf, Stderr: io.Discard})
	}

	now := time.Now().UTC()
	rc := 0
	var report []string
	objs := append(decodePreflightObjs(liveBuf.String()), decodePreflightObjs(nsBuf.String())...)
	for _, o := range objs {
		kind, ns, name := o.Kind, o.Metadata.Namespace, o.Metadata.Name
		dts, fins := o.Metadata.DeletionTimestamp, strings.Join(o.Metadata.Finalizers, ",")
		if kind == "" || name == "" || dts == "" {
			continue
		}
		nsSuffix := ""
		if ns != "" {
			nsSuffix = fmt.Sprintf(" (ns %s)", ns)
		}
		// An UNPARSEABLE timestamp must SKIP the object, not count as old —
		// defaulting to "old" would silently disable the age gate and
		// force-clear legitimately-draining deletions.
		when, err := time.Parse(time.RFC3339, dts)
		if err != nil {
			report = append(report, fmt.Sprintf("%s/%s%s — unparseable deletionTimestamp '%s', not touching it", kind, name, nsSuffix, dts))
			continue
		}
		elapsed := int(now.Sub(when).Seconds())
		if elapsed < age {
			report = append(report, fmt.Sprintf("%s/%s%s deleting only %ds (< %ds) — letting it drain", kind, name, nsSuffix, elapsed, age))
			continue
		}
		if kind == "CustomResourceDefinition" {
			crdReport, ok := a.preflightCRD(ctx, name, crds, crdAllow, kubectlFlags)
			if !ok {
				rc = 1
			}
			report = append(report, crdReport...)
			continue
		}
		if kind == "Namespace" {
			// The caller already gated this force — assert the override for
			// this one call so finalizeNamespace's own confirm doesn't
			// re-ask/refuse.
			forced := *a
			forced.ForceRecreate = true
			forced.finalizeNamespace(ctx, name, kubectlFlags)
			// Re-check the live state so the report never claims success
			// for a namespace still wedged.
			probe := append(append([]string{}, kubectlFlags...),
				"get", "ns", name, "-o", "jsonpath={.metadata.deletionTimestamp}")
			var pb strings.Builder
			_ = a.Runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: probe, Stdout: &pb, Stderr: io.Discard})
			if strings.TrimSpace(pb.String()) != "" {
				report = append(report, fmt.Sprintf("could not finalize namespace/%s — still terminating", name))
				rc = 1
			} else {
				report = append(report, fmt.Sprintf("namespace/%s patched", name))
				report = append(report, fmt.Sprintf("force-finalized Namespace/%s — stuck %dm%s", name, elapsed/60, finsSuffix(fins)))
			}
			continue
		}
		// Fully-qualified type (Kind.version.group) — a bare kind is
		// ambiguous the moment two groups share it.
		stuck := kind
		if strings.Contains(o.APIVersion, "/") {
			group, version, _ := strings.Cut(o.APIVersion, "/")
			stuck = kind + "." + version + "." + group
		}
		patch := append(append([]string{}, kubectlFlags...), "patch", stuck, name)
		if ns != "" {
			patch = append(patch, "-n", ns)
		}
		patch = append(patch, "--type", "merge", "-p", `{"metadata":{"finalizers":null}}`)
		if a.kubectlQuiet(ctx, "", patch...) == 0 {
			report = append(report, fmt.Sprintf("%s/%s patched", strings.ToLower(stuck), name))
			report = append(report, fmt.Sprintf("cleared %s/%s%s — stuck %dm%s", kind, name, nsSuffix, elapsed/60, finsSuffix(fins)))
		} else {
			report = append(report, fmt.Sprintf("could not clear finalizers on %s/%s%s", kind, name, nsSuffix))
			rc = 1
		}
	}

	if len(report) > 0 {
		RenderCaptured(a.Stdout, "preflight", rc, strings.NewReader(strings.Join(report, "\n")+"\n"))
	} else {
		// Nothing stuck: one honest line, same glyph family as the block UI.
		cOn, cOff, cDim := "", "", ""
		if writerIsTTY(a.Stdout) {
			cOn, cOff, cDim = "\033[32m", "\033[0m", "\033[2m"
		}
		fmt.Fprintf(a.Stdout, "%s✓%s preflight %s· nothing stuck%s\n", cOn, cOff, cDim, cOff)
	}
	return nil
}

func finsSuffix(fins string) string {
	if fins == "" {
		return ""
	}
	return " · finalizers: " + fins
}

func envDefault(name, def string) string {
	// os.Getenv wrapped so preflight reads its policy env like bash did.
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// preflightCRD handles ONE stuck-Terminating CustomResourceDefinition per
// the --crds policy (bash: kapply::_preflight_crd). Returns report lines
// and ok=false when something stayed wedged.
//
// A terminating CRD is almost never stuck on its OWN finalizer:
// customresourcecleanup waits for the CRD's INSTANCES to go, and the
// instances wait on finalizers owned by an operator that is typically part
// of the very apply this preflight unblocks. Hence:
//
//	skip  — report and stand back (the conservative carve-out).
//	drain — clear finalizers on the INSTANCES only; the CRD's own finalizer
//	        is never touched — cleanup then completes naturally.
//	force — drain first; if the CRD is STILL wedged after the bounded wait,
//	        strip its finalizer too (restricted to allow when set).
func (a *Applier) preflightCRD(ctx context.Context, name, mode, allow string, kubectlFlags []string) ([]string, bool) {
	if mode == "skip" {
		return []string{fmt.Sprintf("refusing to force stuck CustomResourceDefinition/%s — crds policy is 'skip' (clearing it would cascade-delete every CR of that kind); resolve by hand", name)}, true
	}

	ok := true
	drained, failed := 0, 0
	var report []string
	listArgs := append(append([]string{}, kubectlFlags...), "get", name, "-A", "-o", "json")
	var listBuf strings.Builder
	_ = a.Runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: listArgs, Stdout: &listBuf, Stderr: io.Discard})
	var instances struct {
		Items []preflightObj `json:"items"`
	}
	_ = json.Unmarshal([]byte(listBuf.String()), &instances)
	for _, inst := range instances.Items {
		iname := inst.Metadata.Name
		if iname == "" {
			continue
		}
		patch := append(append([]string{}, kubectlFlags...), "patch", name, iname)
		if inst.Metadata.Namespace != "" {
			patch = append(patch, "-n", inst.Metadata.Namespace)
		}
		patch = append(patch, "--type", "merge", "-p", `{"metadata":{"finalizers":null}}`)
		if a.kubectlQuiet(ctx, "", patch...) == 0 {
			drained++
		} else {
			report = append(report, fmt.Sprintf("could not clear finalizers on %s/%s", name, iname))
			failed++
			ok = false
		}
	}

	// With the instances gone, customresourcecleanup completes near-instantly.
	crdWait := envInt("KAPPLY_CRD_WAIT", 20)
	gone := false
	for i := 0; i < crdWait; i++ {
		probe := append(append([]string{}, kubectlFlags...), "get", "crd", name)
		if a.kubectlQuiet(ctx, "", probe...) != 0 {
			gone = true
			break
		}
		a.sleep(a.pollInterval())
	}
	if !gone {
		probe := append(append([]string{}, kubectlFlags...), "get", "crd", name)
		gone = a.kubectlQuiet(ctx, "", probe...) != 0
	}
	if gone {
		report = append(report, fmt.Sprintf("customresourcedefinition/%s patched", name))
		report = append(report, fmt.Sprintf("drained %d instance(s) of CustomResourceDefinition/%s — cleanup completed on its own", drained, name))
		return report, ok
	}

	if mode == "force" && crdAllowed(name, allow) {
		patch := append(append([]string{}, kubectlFlags...), "patch", "crd", name,
			"--type", "merge", "-p", `{"metadata":{"finalizers":null}}`)
		if a.kubectlQuiet(ctx, "", patch...) == 0 {
			report = append(report, fmt.Sprintf("customresourcedefinition/%s patched", name))
			report = append(report, fmt.Sprintf("FORCED CustomResourceDefinition/%s finalizer off after drain (%d cleared, %d failed) — stale etcd CRs may resurrect on re-apply", name, drained, failed))
		} else {
			report = append(report, fmt.Sprintf("could not force finalizers on CustomResourceDefinition/%s", name))
			ok = false
		}
		return report, ok
	}

	notInAllow := ""
	if mode == "force" {
		notInAllow = ", not in crd-allow"
	}
	report = append(report, fmt.Sprintf("CustomResourceDefinition/%s still terminating after draining %d instance(s) — not forcing its finalizer (crds policy: %s%s)", name, drained, mode, notInAllow))
	return report, false
}

// crdAllowed: empty allowlist = every manifest CRD may be forced; otherwise
// exact match, with whitespace around csv entries stripped (bash:
// kapply::_crd_allowed).
func crdAllowed(name, allow string) bool {
	allow = strings.ReplaceAll(allow, " ", "")
	if allow == "" {
		return true
	}
	return strings.Contains(","+allow+",", ","+name+",")
}
