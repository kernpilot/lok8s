package kapply

// captured_test.go — the Go port of the kapply::render_captured bats block
// (offline collapsed blocks for buffered bootstrap jobs). All writers are
// buffers → the off-tty (plain) path, exactly what the bats pipe asserted.

import (
	"bytes"
	"strings"
	"testing"
)

func render(t *testing.T, label string, rc int, in string) string {
	t.Helper()
	var buf bytes.Buffer
	RenderCaptured(&buf, label, rc, strings.NewReader(in))
	return buf.String()
}

func TestRenderCapturedCollapsesRoutineLines(t *testing.T) {
	out := render(t, "cert-manager", 0, `namespace/cert-manager serverside-applied
deployment.apps/cert-manager serverside-applied
customresourcedefinition.apiextensions.k8s.io/certificates.cert-manager.io condition met
`)
	if !strings.Contains(out, "✓") || !strings.Contains(out, "cert-manager") || !strings.Contains(out, "· 3 resources") {
		t.Errorf("bad summary: %q", out)
	}
	// the whole point: the routine per-object lines are collapsed away
	if strings.Contains(out, "serverside-applied") || strings.Contains(out, "condition met") {
		t.Errorf("routine lines leaked: %q", out)
	}
}

func TestRenderCapturedFailureSurfacesDedupedErrors(t *testing.T) {
	out := render(t, "networking", 1, `secret/kubehz-tls serverside-applied
no matches for kind "GatewayClass" in version "gateway.networking.k8s.io/v1"
no matches for kind "GatewayClass" in version "gateway.networking.k8s.io/v1"
`)
	if !strings.Contains(out, "✗ networking · 1 resource") {
		t.Errorf("bad header: %q", out)
	}
	if !strings.Contains(out, `no matches for kind "GatewayClass"`) || !strings.Contains(out, "×2") {
		t.Errorf("errors not surfaced/deduped: %q", out)
	}
}

func TestRenderCapturedKeepsFinalLineWithoutNewline(t *testing.T) {
	// a killed job's buffer can end mid-write; a plain line-read would drop it
	out := render(t, "broken", 1, "secret/x serverside-applied\npartial error, no newline")
	if !strings.Contains(out, "partial error, no newline") {
		t.Errorf("final unterminated line dropped: %q", out)
	}
}

func TestRenderCapturedZeroResourcesBareHeader(t *testing.T) {
	out := render(t, "cilium", 0, "[bootstrap] cilium already deployed by the KubeOne driver — skipping\n")
	if !strings.Contains(out, "✓") || !strings.Contains(out, "cilium") {
		t.Errorf("bad header: %q", out)
	}
	if strings.Contains(out, "resource") {
		t.Errorf("count rendered for zero resources: %q", out)
	}
	if !strings.Contains(out, "already deployed") {
		t.Errorf("skip notice lost: %q", out)
	}
}

func TestRenderCapturedRetryPassesCountOnce(t *testing.T) {
	// the CRD-race retry re-applies the whole manifest — the summary must
	// count distinct resources, not lines-per-pass
	out := render(t, "cnpg", 0, `namespace/cnpg-system serverside-applied
deployment.apps/cnpg serverside-applied
namespace/cnpg-system serverside-applied
deployment.apps/cnpg serverside-applied
customresourcedefinition.apiextensions.k8s.io/clusters.cnpg.io condition met
`)
	if !strings.Contains(out, "· 3 resources") {
		t.Errorf("count inflated: %q", out)
	}
}

func TestRenderCapturedOffTTYIsPlain(t *testing.T) {
	out := render(t, "networking", 1, "secret/x serverside-applied\n\033[31mcolored controller error\033[0m\n")
	if strings.Contains(out, "\033") {
		t.Errorf("escape sequences leaked: %q", out)
	}
	if !strings.Contains(out, "✗ networking · 1 resource") {
		t.Errorf("bad plain header: %q", out)
	}
	if !strings.Contains(out, "colored controller error") {
		t.Errorf("payload lost: %q", out)
	}
}

func TestRenderCapturedIndentedOKVerbSurfaces(t *testing.T) {
	// an empty first token was a FATAL bad-subscript in bash — here it must
	// surface as a message, never count
	out := render(t, "weird", 0, "secret/x serverside-applied\n    patched\n")
	if !strings.Contains(out, "· 1 resource") {
		t.Errorf("bad count: %q", out)
	}
	if !strings.Contains(out, "patched") {
		t.Errorf("indented line lost: %q", out)
	}
}

func TestRenderCapturedProseEndingInOKVerbSurfaces(t *testing.T) {
	out := render(t, "warn", 0, `namespace/x serverside-applied
Warning: resource configmaps/y lacks the last-applied annotation and was configured
`)
	if !strings.Contains(out, "· 1 resource") {
		t.Errorf("prose counted as a resource: %q", out)
	}
	if !strings.Contains(out, "Warning: resource configmaps/y") {
		t.Errorf("warning lost: %q", out)
	}
}

func TestRenderCapturedStripsCarriageReturns(t *testing.T) {
	out := render(t, "cr-test", 1, "secret/x serverside-applied\nprogress line\rovertyped error\n")
	if strings.Contains(out, "\r") {
		t.Errorf("carriage return leaked: %q", out)
	}
	if !strings.Contains(out, "overtyped error") {
		t.Errorf("payload lost: %q", out)
	}
}
