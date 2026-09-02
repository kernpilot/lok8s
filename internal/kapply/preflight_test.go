package kapply

// preflight_test.go — the Go port of the kapply::preflight bats block: the
// pre-apply sweep for stuck-Terminating objects. The stub distinguishes the
// GET shapes so the batched manifest path is actually exercised (a
// regression that stopped feeding the manifest to kubectl could not stay
// green on the referenced-namespace fetch alone).

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/execx"
)

// preflightStub mirrors the bats _preflight_stub, including the stateful
// namespace-finalize and CRD-drain behaviors.
type preflightStub struct {
	calls        []string
	getOut       string // answer to `get -f -`
	nsGetOut     string // answer to `get ns|namespace …`
	crdName      string
	crdInstances string // answer to `get <crd-name> -A`
	crdStays     bool   // keep the CRD wedged after the drain
	nsFinalizeOK bool   // after /finalize the namespace reads as GONE
	patchRC      int

	nsFinalized bool
	crdDrained  bool
}

func (f *preflightStub) log() string { return strings.Join(f.calls, "\n") }

func (f *preflightStub) Run(ctx context.Context, c execx.Cmd) error {
	f.calls = append(f.calls, c.Name+" "+strings.Join(c.Args, " "))
	args := c.Args
	if len(args) == 0 {
		return nil
	}
	out := func(s string) {
		if c.Stdout != nil {
			fmt.Fprintln(c.Stdout, s)
		}
	}
	switch args[0] {
	case "get":
		if len(args) > 1 {
			switch args[1] {
			case "-f":
				out(f.getOut)
			case "ns", "namespace":
				if f.nsFinalizeOK && f.nsFinalized {
					// namespace reads as GONE after the finalize
				} else {
					// jsonpath probes get just the deletionTimestamp; JSON
					// probes get the whole object. The engine only greps
					// non-emptiness or parses JSON — answer with whichever
					// the flags asked for.
					if hasArg(args, "-o", "jsonpath={.metadata.deletionTimestamp}") {
						if f.nsGetOut != "" {
							out("2020-01-01T00:00:00Z")
						}
					} else {
						out(f.nsGetOut)
					}
				}
			case "crd":
				if !f.crdStays && f.crdDrained {
					return &rcError{1} // cleanup completed → CRD gone
				}
				return nil
			case f.crdName:
				out(f.crdInstances)
			default:
				out(f.getOut)
			}
		}
		return nil
	case "replace":
		f.nsFinalized = true
		return nil
	case "patch":
		if len(args) > 1 && args[1] == f.crdName {
			f.crdDrained = true
		}
		if f.patchRC != 0 {
			return &rcError{f.patchRC}
		}
		return nil
	default:
		return nil
	}
}

func hasArg(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

func preflightApplier(f *preflightStub) (*Applier, *bytes.Buffer) {
	var out bytes.Buffer
	a := &Applier{Runner: f, Stdout: &out, Stderr: &out,
		NonInteractive: true, NsWait: 0,
		Sleep: func(time.Duration) {}, PollInterval: time.Nanosecond}
	return a, &out
}

func stuckCRDFixture(f *preflightStub) {
	f.crdName = "kubehzclusters.kubehz.dev"
	f.getOut = `{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"kubehzclusters.kubehz.dev","deletionTimestamp":"2020-01-01T00:00:00Z","finalizers":["customresourcecleanup.apiextensions.k8s.io"]}}`
	f.crdInstances = `{"items":[
    {"metadata":{"namespace":"kubehz-system","name":"k6-burst-1","finalizers":["kubehz.dev/cluster-cleanup"]}},
    {"metadata":{"namespace":"kubehz-system","name":"k6-burst-2","finalizers":["kubehz.dev/cluster-cleanup"]}}]}`
}

func TestPreflightNothingTerminating(t *testing.T) {
	f := &preflightStub{getOut: `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data","namespace":"app"}}`}
	a, out := preflightApplier(f)
	if err := a.Preflight(context.Background(), deployManifest); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !strings.Contains(out.String(), "nothing stuck") {
		t.Errorf("missing nothing-stuck line: %q", out.String())
	}
	if strings.Contains(f.log(), " patch ") {
		t.Errorf("unexpected patch:\n%s", f.log())
	}
}

func TestPreflightClearsStuckObjectWithQualifiedType(t *testing.T) {
	f := &preflightStub{getOut: `{"apiVersion":"postgresql.cnpg.io/v1","kind":"Database","metadata":{"name":"status","namespace":"kubehz-system","deletionTimestamp":"2020-01-01T00:00:00Z","finalizers":["cnpg.io/deleteDatabase"]}}`}
	a, out := preflightApplier(f)
	_ = a.Preflight(context.Background(), crManifest)
	if !strings.Contains(out.String(), "cnpg.io/deleteDatabase") {
		t.Errorf("finalizer not reported: %q", out.String())
	}
	// The stuck object must have come in via the BATCHED manifest get…
	if !strings.Contains(f.log(), "get -f - -o json --ignore-not-found") {
		t.Errorf("no batched manifest get:\n%s", f.log())
	}
	// …and go out via a fully-qualified patch (a bare kind is ambiguous).
	if !strings.Contains(f.log(), "patch Database.v1.postgresql.cnpg.io status -n kubehz-system --type merge") {
		t.Errorf("no qualified patch:\n%s", f.log())
	}
}

func TestPreflightYoungDeletionLeftToDrain(t *testing.T) {
	fresh := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	f := &preflightStub{getOut: `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data","namespace":"app","deletionTimestamp":"` + fresh + `","finalizers":["kubernetes.io/pvc-protection"]}}`}
	a, out := preflightApplier(f)
	_ = a.Preflight(context.Background(), deployManifest)
	if !strings.Contains(out.String(), "letting it drain") {
		t.Errorf("young deletion not drained: %q", out.String())
	}
	if strings.Contains(f.log(), " patch ") {
		t.Errorf("young deletion was force-cleared:\n%s", f.log())
	}
}

func TestPreflightUnparseableTimestampSkipped(t *testing.T) {
	f := &preflightStub{getOut: `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data","namespace":"app","deletionTimestamp":"not-a-time","finalizers":["kubernetes.io/pvc-protection"]}}`}
	a, out := preflightApplier(f)
	_ = a.Preflight(context.Background(), deployManifest)
	if !strings.Contains(out.String(), "unparseable deletionTimestamp") {
		t.Errorf("missing skip report: %q", out.String())
	}
	if strings.Contains(f.log(), " patch ") {
		t.Errorf("unparseable timestamp was force-cleared:\n%s", f.log())
	}
}

func TestPreflightReferencedNamespaceStillWedgedReported(t *testing.T) {
	// The namespace is REFERENCED by the manifest (ns default), not
	// declared in it — this pins the second fetch path. The static stub
	// keeps answering with a deletionTimestamp after the finalize, so the
	// honesty re-check must report the wedge, not "patched".
	f := &preflightStub{nsGetOut: `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"mla","deletionTimestamp":"2020-01-01T00:00:00Z","finalizers":["kubernetes"]}}`}
	a, out := preflightApplier(f)
	_ = a.Preflight(context.Background(), deployManifest)
	if !strings.Contains(out.String(), "could not finalize namespace/mla") {
		t.Errorf("wedge not reported honestly: %q", out.String())
	}
	if !strings.Contains(f.log(), "replace --raw /api/v1/namespaces/mla/finalize") {
		t.Errorf("no /finalize call:\n%s", f.log())
	}
}

func TestPreflightFinalizedNamespaceReportsSuccess(t *testing.T) {
	f := &preflightStub{
		nsGetOut:     `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"mla","deletionTimestamp":"2020-01-01T00:00:00Z","finalizers":["kubernetes"]}}`,
		nsFinalizeOK: true,
	}
	a, out := preflightApplier(f)
	_ = a.Preflight(context.Background(), deployManifest)
	if !strings.Contains(out.String(), "force-finalized Namespace/mla") {
		t.Errorf("success not reported: %q", out.String())
	}
	if strings.Contains(out.String(), "could not finalize") {
		t.Errorf("false wedge report: %q", out.String())
	}
}

func TestPreflightCRDDrainsInstancesNeverTouchesCRDFinalizer(t *testing.T) {
	f := &preflightStub{}
	stuckCRDFixture(f)
	a, out := preflightApplier(f)
	_ = a.Preflight(context.Background(), crManifest)
	if !strings.Contains(out.String(), "drained 2 instance(s) of CustomResourceDefinition/kubehzclusters.kubehz.dev") {
		t.Errorf("drain not reported: %q", out.String())
	}
	if !strings.Contains(f.log(), "patch kubehzclusters.kubehz.dev k6-burst-1 -n kubehz-system") ||
		!strings.Contains(f.log(), "patch kubehzclusters.kubehz.dev k6-burst-2 -n kubehz-system") {
		t.Errorf("instances not drained:\n%s", f.log())
	}
	if strings.Contains(f.log(), "patch crd ") {
		t.Errorf("CRD finalizer touched under drain:\n%s", f.log())
	}
}

func TestPreflightCRDSkipPolicyRefuses(t *testing.T) {
	f := &preflightStub{}
	stuckCRDFixture(f)
	a, out := preflightApplier(f)
	_ = a.Preflight(context.Background(), crManifest, "--crds", "skip")
	if !strings.Contains(out.String(), "refusing to force stuck CustomResourceDefinition/kubehzclusters.kubehz.dev") {
		t.Errorf("skip policy not honored: %q", out.String())
	}
	if strings.Contains(f.log(), " patch ") {
		t.Errorf("skip policy patched something:\n%s", f.log())
	}
}

func TestPreflightCRDDrainStillWedgedDoesNotEscalate(t *testing.T) {
	f := &preflightStub{crdStays: true}
	stuckCRDFixture(f)
	a, out := preflightApplier(f)
	_ = a.Preflight(context.Background(), crManifest)
	if !strings.Contains(out.String(), "still terminating after draining 2 instance(s)") {
		t.Errorf("wedge not reported: %q", out.String())
	}
	if strings.Contains(f.log(), "patch crd ") {
		t.Errorf("drain escalated to CRD finalizer:\n%s", f.log())
	}
}

func TestPreflightCRDForceEscalates(t *testing.T) {
	f := &preflightStub{crdStays: true}
	stuckCRDFixture(f)
	a, out := preflightApplier(f)
	_ = a.Preflight(context.Background(), crManifest, "--crds", "force")
	if !strings.Contains(out.String(), "FORCED CustomResourceDefinition/kubehzclusters.kubehz.dev") {
		t.Errorf("force not reported: %q", out.String())
	}
	if !strings.Contains(f.log(), "patch crd kubehzclusters.kubehz.dev") {
		t.Errorf("no CRD finalizer patch:\n%s", f.log())
	}
}

func TestPreflightCRDForceAllowlistWhitespace(t *testing.T) {
	f := &preflightStub{crdStays: true}
	stuckCRDFixture(f)
	a, out := preflightApplier(f)
	_ = a.Preflight(context.Background(), crManifest, "--crds", "force",
		"--crd-allow", "other.example.com, kubehzclusters.kubehz.dev")
	if !strings.Contains(out.String(), "FORCED CustomResourceDefinition/kubehzclusters.kubehz.dev") {
		t.Errorf("csv whitespace not tolerated: %q", out.String())
	}
}

func TestPreflightCRDForceRespectsAllowlist(t *testing.T) {
	f := &preflightStub{crdStays: true}
	stuckCRDFixture(f)
	a, out := preflightApplier(f)
	_ = a.Preflight(context.Background(), crManifest, "--crds", "force",
		"--crd-allow", "other.example.com")
	if !strings.Contains(out.String(), "not forcing its finalizer") {
		t.Errorf("allowlist ignored: %q", out.String())
	}
	if strings.Contains(f.log(), "patch crd ") {
		t.Errorf("non-listed CRD stripped:\n%s", f.log())
	}
}

func TestPreflightNonNumericCRDWaitFallsBack(t *testing.T) {
	t.Setenv("KAPPLY_CRD_WAIT", "20s")
	f := &preflightStub{}
	stuckCRDFixture(f)
	a, out := preflightApplier(f)
	_ = a.Preflight(context.Background(), crManifest)
	if !strings.Contains(out.String(), "drained 2 instance(s)") {
		t.Errorf("non-numeric wait aborted the report: %q", out.String())
	}
}

func TestPreflightFailedPatchReportsAndExitsZero(t *testing.T) {
	f := &preflightStub{
		getOut:  `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data","namespace":"app","deletionTimestamp":"2020-01-01T00:00:00Z"}}`,
		patchRC: 1,
	}
	a, out := preflightApplier(f)
	if err := a.Preflight(context.Background(), deployManifest); err != nil {
		t.Fatalf("preflight must never fail the deploy it protects: %v", err)
	}
	if !strings.Contains(out.String(), "could not clear finalizers on PersistentVolumeClaim/data") {
		t.Errorf("failure not reported: %q", out.String())
	}
}
