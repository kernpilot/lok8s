package kubehz

// cluster_test.go covers the cluster verbs in cluster.go: status, the
// claim flow (nonce, code), re-enroll, assess and the assessment printer.
// It ports tests/unit/kubehz_claim_test.bats and the assess cases of
// kubehz_handover_test.bats. The api is an httptest TLS server; kubectl is
// the fake Runner.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

func TestStatusAccessNone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeSpec("test.kubehz.dev", specYAML("KubeOne", "    access:\n"))
	mustOK(t, h.ctx.Status(t.Context(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "not registered (access: none)")
	mustContain(t, h.output(), "API URL: <not set>")
}

func registeredSpec(h *harness) {
	h.writeSpec("test.kubehz.dev", specYAML("KubeOne", "    access: registered\n    apiUrl: "+h.apiURL()+"\n"))
	h.env["KUBEHZ_TOKEN"] = "test-token"
}

func TestStatusReadsRowNotEnvelope(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	registeredSpec(h)
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[
  {"id":"cl-new","domain":"test.kubehz.dev","status":"Creating","createdAt":"2026-02-01T00:00:00Z"},
  {"id":"cl-old","domain":"test.kubehz.dev","status":"Running","lastHeartbeat":"2026-08-19T10:00:00Z","connected":true,"createdAt":"2026-01-01T00:00:00Z"},
  {"id":"cl-other","domain":"other.example.com","status":"Error","createdAt":"2026-03-01T00:00:00Z"}
],"meta":{"page":1,"perPage":500,"total":3}}`)
	mustOK(t, h.ctx.Status(t.Context(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "Agent:   cronjob (cronjob/kubehz-heartbeat — every 5 minutes)")
	mustContain(t, h.output(), "Status:  Running (id: cl-old)")
	mustContain(t, h.output(), "Beat:    2026-08-19T10:00:00Z (connected)")
	mustNotContain(t, h.output(), "Status:  unknown")
}

func TestStatusNoRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	registeredSpec(h)
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[{"id":"cl-other","domain":"other.example.com","status":"Running","createdAt":"2026-03-01T00:00:00Z"}]}`)
	mustOK(t, h.ctx.Status(t.Context(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "not registered (no cluster for test.kubehz.dev")
}

func TestStatusNon2xxIsUnknown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	registeredSpec(h)
	h.handle("GET /api/clusters", 401, `{"ok":false}`)
	mustOK(t, h.ctx.Status(t.Context(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "unknown (HTTP 401")
}

func TestStatusStaleBeatAndNoBeat(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	registeredSpec(h)
	h.handle("GET /api/clusters", 200, `{"data":[{"id":"cl-1","domain":"test.kubehz.dev","status":"Running","lastHeartbeat":"2026-01-01T00:00:00Z","connected":false}]}`)
	mustOK(t, h.ctx.Status(t.Context(), "test.kubehz.dev"), h.output())
	// jq's `.connected // empty` erases a JSON false, so the bash prints the
	// bare beat line for a stale row (quirk preserved; see the report).
	mustContain(t, h.output(), "Beat:    2026-01-01T00:00:00Z\n")
	mustNotContain(t, h.output(), "stale")
	h.reset()
	h.handle("GET /api/clusters", 200, `{"data":[{"id":"cl-1","domain":"test.kubehz.dev","status":"Running","lastHeartbeat":"2026-01-01T00:00:00Z","connected":"false"}]}`)
	mustOK(t, h.ctx.Status(t.Context(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "Beat:    2026-01-01T00:00:00Z (stale — outside the reporting window)")
	h.reset()
	h.handle("GET /api/clusters", 200, `{"data":[{"id":"cl-1","domain":"test.kubehz.dev","status":"Pending"}]}`)
	mustOK(t, h.ctx.Status(t.Context(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "Beat:    none yet — deploy the heartbeat agent")
}

func TestRegisterAndDeregisterSubcommandsRejectAccessNone(t *testing.T) {
	h := newHarness(t)
	h.writeSpec("test.kubehz.dev", "kind: Lo\nspec:\n  kubehz:\n    access:\n")
	mustErr(t, h.ctx.Register(t.Context(), "test.kubehz.dev"))
	mustContain(t, h.output(), "access is 'none'")
	h.reset()
	mustErr(t, h.ctx.Deregister(t.Context(), "test.kubehz.dev"))
	mustContain(t, h.output(), "access is 'none'")
}

func kubectlStub(h *harness, cmMissing, secretMissing bool) {
	h.runner.handler = func(c execx.Cmd, _ string) error {
		line := argvLine(c)
		switch {
		case strings.Contains(line, "get configmap kubehz-agent-config"):
			if cmMissing {
				return exitErr(1)
			}
		case strings.Contains(line, "get secret kubehz-agent"):
			if secretMissing {
				return exitErr(1)
			}
			if strings.Contains(line, "jsonpath") {
				io.WriteString(c.Stdout, base64.StdEncoding.EncodeToString([]byte("khz_agt_bats")))
			}
		}
		return nil
	}
}

func TestClaimRejectsMalformedNonce(t *testing.T) {
	h := newHarness(t)
	kubectlStub(h, false, false)
	mustErr(t, h.ctx.Claim(t.Context(), "khzt_wrong_prefix_value_000000"))
	mustContain(t, h.output(), "invalid claim nonce")
	mustNotContain(t, h.output(), "khzt_wrong_prefix_value_000000")
	if len(h.runner.calls) != 0 {
		t.Fatal("no kubectl call may run")
	}
	h.reset()
	mustErr(t, h.ctx.Claim(t.Context(), "khzn_short"))
	mustContain(t, h.output(), "invalid claim nonce")
}

func TestClaimPlacesNonceInOneAnnotateCall(t *testing.T) {
	h := newHarness(t)
	kubectlStub(h, false, false)
	nonce := "khzn_batsPlacedNonce_43charsBase64urlValue000"
	mustOK(t, h.ctx.Claim(t.Context(), nonce), h.output())
	mustContain(t, h.output(), "claim nonce placed")
	mustContain(t, h.output(), "15 minutes")
	mustNotContain(t, h.output(), nonce)
	if h.runner.countCalls("annotate") != 1 {
		t.Fatalf("annotate calls: %v", h.runner.lines())
	}
	var ann string
	for _, l := range h.runner.lines() {
		if strings.Contains(l, "annotate") {
			ann = l
		}
	}
	for _, want := range []string{"kubehz.cloud/claim-nonce=" + nonce, "kubehz.cloud/claim-nonce-placed=1700000000", "--overwrite"} {
		mustContain(t, ann, want)
	}
}

// ClaimNonce keeps the ticket off argv: `-` reads one line from stdin,
// an empty flag falls back to KUBEHZ_CLAIM_NONCE, and nothing supplied is
// reported as "" (the caller prints the argsh missing-flag refusal).
func TestClaimNonceFlagStdinAndEnv(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	got, err := h.ctx.ClaimNonce("khzn_fromflag_000000000000000000", strings.NewReader("khzn_ignored_stdin_0000000000000\n"))
	if err != nil || got != "khzn_fromflag_000000000000000000" {
		t.Fatalf("flag: %q %v", got, err)
	}
	got, err = h.ctx.ClaimNonce("-", strings.NewReader("  khzn_fromstdin_00000000000000000  \nsecond line\n"))
	if err != nil || got != "khzn_fromstdin_00000000000000000" {
		t.Fatalf("stdin: %q %v", got, err)
	}
	got, err = h.ctx.ClaimNonce("-", strings.NewReader("khzn_noNewline_000000000000000000"))
	if err != nil || got != "khzn_noNewline_000000000000000000" {
		t.Fatalf("stdin without newline: %q %v", got, err)
	}
	h.env[ClaimNonceEnv] = "khzn_fromenv_0000000000000000000"
	got, err = h.ctx.ClaimNonce("", strings.NewReader("khzn_ignored_stdin_0000000000000\n"))
	if err != nil || got != "khzn_fromenv_0000000000000000000" {
		t.Fatalf("env: %q %v", got, err)
	}
	delete(h.env, ClaimNonceEnv)
	got, err = h.ctx.ClaimNonce("", strings.NewReader(""))
	if err != nil || got != "" {
		t.Fatalf("nothing supplied: %q %v", got, err)
	}
	// `-` with nothing on stdin is an error, and it names the cause.
	h.reset()
	_, err = h.ctx.ClaimNonce("-", strings.NewReader("\n"))
	mustErr(t, err)
	mustContain(t, h.output(), "no claim nonce on stdin")
}

func TestClaimMissingConfigMap(t *testing.T) {
	h := newHarness(t)
	kubectlStub(h, true, false)
	mustErr(t, h.ctx.Claim(t.Context(), "khzn_batsPlacedNonce_43charsBase64urlValue000"))
	mustContain(t, h.output(), "not found")
	mustContain(t, h.output(), "Deploy the heartbeat agent")
}

func TestClaimCodePrintsCNeverA(t *testing.T) {
	h := newHarness(t)
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if strings.Contains(argvLine(c), "jsonpath={.data.claim-code}") {
			io.WriteString(c.Stdout, base64.StdEncoding.EncodeToString([]byte("khzc_test_code")))
		}
		return nil
	}
	mustOK(t, h.ctx.ClaimCode(t.Context()), h.output())
	if h.out.String() != "khzc_test_code\n" {
		t.Fatalf("stdout = %q", h.out.String())
	}
	mustContain(t, h.errOut.String(), "Paste this one-time claim code")
	mustNotContain(t, h.output(), "khz_agt_")
}

func TestClaimCodeSecretAbsent(t *testing.T) {
	h := newHarness(t)
	h.runner.handler = func(c execx.Cmd, _ string) error { return exitErr(1) }
	mustErr(t, h.ctx.ClaimCode(t.Context()))
	mustContain(t, h.output(), "secret/kubehz-agent not found in kubehz-system")
}

func reEnrollSpec(h *harness, apiURL string) {
	h.writeSpec("test.kubehz.dev", specYAML("KubeOne", "    access: registered\n    apiUrl: "+apiURL+"\n"))
	h.env["KUBEHZ_TOKEN"] = "khzt_bats"
	h.handle("GET /api/clusters", 200, `{"data":[{"id":"cl-999","domain":"other.kubehz.dev"},{"id":"cl-123","domain":"test.kubehz.dev"}]}`)
}

func TestReEnrollHashesAndReportsRotated(t *testing.T) {
	h := newHarness(t)
	reEnrollSpec(h, h.apiURL())
	kubectlStub(h, false, false)
	h.handleFunc("POST /api/clusters/cl-123/agent-token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer khzt_bats" {
			t.Fatal("agent-token POST missing the user bearer")
		}
		io.WriteString(w, `{"rotated":true,"clusterId":"cl-123"}`)
	})
	mustOK(t, h.ctx.ReEnroll(t.Context(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "agent token re-enrolled for test.kubehz.dev (cl-123)")
	mustContain(t, h.output(), "heartbeats resume")
	mustNotContain(t, h.output(), "khz_agt_bats")
	sum := sha256.Sum256([]byte("khz_agt_bats"))
	var body map[string]string
	_ = json.Unmarshal([]byte(h.lastReq("POST", "/agent-token").Body), &body)
	if body["agentTokenHash"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash = %q", body["agentTokenHash"])
	}
}

func TestReEnrollAlreadyLive(t *testing.T) {
	h := newHarness(t)
	reEnrollSpec(h, h.apiURL())
	kubectlStub(h, false, false)
	h.handle("POST /api/clusters/cl-123/agent-token", 200, `{"rotated":false,"clusterId":"cl-123"}`)
	mustOK(t, h.ctx.ReEnroll(t.Context(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "already the live one")
	mustNotContain(t, h.output(), "re-enrolled for")
}

func TestReEnrollConflict(t *testing.T) {
	h := newHarness(t)
	reEnrollSpec(h, h.apiURL())
	kubectlStub(h, false, false)
	h.handle("POST /api/clusters/cl-123/agent-token", 409, `{"statusCode":409,"data":{"code":"AGENT_TOKEN_CONFLICT","message":"That agent token is already registered"}}`)
	mustErr(t, h.ctx.ReEnroll(t.Context(), "test.kubehz.dev"))
	mustContain(t, h.output(), "re-enroll refused (HTTP 409 AGENT_TOKEN_CONFLICT): That agent token is already registered")
}

func TestReEnrollPreconditions(t *testing.T) {
	t.Run("requires token", func(t *testing.T) {
		h := newHarness(t)
		reEnrollSpec(h, h.apiURL())
		delete(h.env, "KUBEHZ_TOKEN")
		mustErr(t, h.ctx.ReEnroll(t.Context(), "test.kubehz.dev"))
		mustContain(t, h.output(), "KUBEHZ_TOKEN is required")
	})
	t.Run("missing secret", func(t *testing.T) {
		h := newHarness(t)
		reEnrollSpec(h, h.apiURL())
		kubectlStub(h, false, true)
		mustErr(t, h.ctx.ReEnroll(t.Context(), "test.kubehz.dev"))
		mustContain(t, h.output(), "no agent-token")
	})
	t.Run("plain http", func(t *testing.T) {
		h := newHarness(t)
		reEnrollSpec(h, "http://api.kubehz.dev")
		mustErr(t, h.ctx.ReEnroll(t.Context(), "test.kubehz.dev"))
		mustContain(t, h.output(), "must use HTTPS")
		if len(h.reqs()) != 0 {
			t.Fatal("no request over plain HTTP")
		}
	})
	t.Run("unresolvable id", func(t *testing.T) {
		h := newHarness(t)
		reEnrollSpec(h, h.apiURL())
		kubectlStub(h, false, false)
		h.handle("GET /api/clusters", 200, `{"data":[]}`)
		mustErr(t, h.ctx.ReEnroll(t.Context(), "test.kubehz.dev"))
		mustContain(t, h.output(), "no cluster found for test.kubehz.dev")
	})
	t.Run("shared and none", func(t *testing.T) {
		h := newHarness(t)
		h.writeSpec("test.kubehz.dev", specYAML("Kubehz", "    hosting: shared\n    apiUrl: https://x\n"))
		mustErr(t, h.ctx.ReEnroll(t.Context(), "test.kubehz.dev"))
		mustContain(t, h.output(), "no heartbeat agent to re-enroll")
	})
}

func TestRenderAssessmentAlignedProbes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	response := `{"ok":true,"data":{"assessedAt":"2026-07-29T09:00:00Z","assessment":{"collectedAt":"2026-07-29T08:58:00Z","k8sVersion":"v1.33.2","datastore":"etcd","etcdReachable":true,"capiManaged":true,"cni":"cilium","podCidr":"10.244.0.0/16","serviceCidr":"10.96.0.0/12","storageClasses":[{"name":"hcloud-volumes","provisioner":"csi.hetzner.cloud","isDefault":true}],"pvSummary":{"count":2,"totalGi":40,"byProvisioner":{"csi.hetzner.cloud":{"count":2,"totalGi":40}}},"loadBalancers":1,"webhooks":{"validating":3,"mutating":1},"cpUsage":{"nodes":5,"cpNodes":3,"etcdDbBytes":null}},"feasibility":{"path":"restore","reasons":["datastore=etcd and a snapshot is obtainable"],"warnings":["1 provider-coupled StorageClass (csi.hetzner.cloud)"]}}}`
	mustOK(t, h.ctx.RenderAssessment("test.kubehz.dev", []byte(response)), h.output())
	for _, want := range []string{
		"kubehz assessment — test.kubehz.dev", "collected 2026-07-29T08:58:00Z", "v1.33.2", "etcd · reachable",
		"pause the CAPI controllers", "cilium", "pods 10.244.0.0/16 · services 10.96.0.0/12",
		"1 classes · 2 PVs · 40Gi", "csi.hetzner.cloud — 2 PVs · 40Gi", "3 validating · 1 mutating",
		"5 total · 3 control-plane", "Feasibility: restore", "datastore=etcd and a snapshot is obtainable",
		"provider-coupled StorageClass",
	} {
		mustContain(t, h.output(), want)
	}
	mustContain(t, h.output(), "✓ kubernetes     v1.33.2")
}

func TestRenderAssessmentNoneYet(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	mustOK(t, h.ctx.RenderAssessment("test.kubehz.dev", []byte(`{"data":{"assessment":null,"assessedAt":null,"feasibility":null}}`)), h.output())
	mustContain(t, h.output(), "No assessment recorded for test.kubehz.dev yet")
	mustContain(t, h.output(), "24h")
}

func TestAssessSharedAndFetch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeSpec("sp.dev", specYAML("Kubehz", "    hosting: shared\n    apiUrl: https://x\n"))
	mustOK(t, h.ctx.Assess(t.Context(), "sp.dev"), h.output())
	mustContain(t, h.output(), "Assessment does not apply to hosting: shared")

	h.reset()
	registeredSpec(h)
	h.handle("GET /api/clusters", 200, `{"data":[{"id":"cl-1","domain":"test.kubehz.dev"}]}`)
	h.handle("GET /api/clusters/cl-1/assessment", 200, `{"data":{"assessment":null}}`)
	mustOK(t, h.ctx.Assess(t.Context(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "No assessment recorded")
	if r := h.lastReq("GET", "/assessment"); r.Auth != "Bearer test-token" {
		t.Fatalf("assessment auth: %+v", r)
	}
}
