package kubehz

// hooks_test.go — compile pins for the hook seams (the assignments below
// stop compiling if either side's signature drifts) plus behaviour tests
// for the bash tails the hooks reproduce. The kubeone/capi driver packages'
// files are NOT edited; the drivers are exercised through their exported
// Hooks fields exactly as the dispatch would wire them.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/driver/capi"
	"github.com/kernpilot/lok8s/internal/driver/kubeone"
	"github.com/kernpilot/lok8s/internal/provision"
)

// The pins: each hook value type-checks against the field it feeds.
var (
	_ = provision.Hooks{
		KubehzRegister:   (*Context)(nil).RegisterHook(),
		KubehzDeregister: (*Context)(nil).DeregisterHook(),
	}
	_ = kubeone.Hooks{
		ReadKubehzConfig: (*Context)(nil).ReadConfigHook(),
		ProvisionHosted:  (*Context)(nil).ProvisionHostedHook(),
		DestroyHosted:    (*Context)(nil).DestroyHostedHook(),
	}
	_ = capi.Hooks{
		ReadKubehzConfig: (*Context)(nil).ReadConfigHook(),
		ProvisionHosted:  (*Context)(nil).ProvisionHostedHook(),
		DestroyHosted:    (*Context)(nil).DestroyHostedHook(),
	}
	_ provision.Hooks = (*Context)(nil).ProvisionHooks()
	_ kubeone.Hooks   = (*Context)(nil).KubeoneHooks()
	_ capi.Hooks      = (*Context)(nil).CapiHooks()
)

func TestReadConfigHookReportsHostingAndGuards(t *testing.T) {
	h := newHarness(t)
	spec := h.writeSpec("hosted.dev", specYAML("KubeOne", "    hosting: hosted\n    apiUrl: https://api.kubehz.dev\n"))
	hosting, err := h.ctx.ReadConfigHook()(spec)
	mustOK(t, err, h.output())
	if hosting != "hosted" {
		t.Fatalf("hosting = %q", hosting)
	}
	_, err = h.ctx.ReadConfigHook()(filepath.Join(h.base, "missing.yaml"))
	mustErr(t, err)
	mustContain(t, h.output(), "cannot read cluster spec")
}

func TestRegisterHookTail(t *testing.T) {
	h := newHarness(t)
	delete(h.env, "KUBEHZ_TOKEN")
	// access: none → no registration, no request.
	spec := h.writeSpec("none.dev", "kind: Lo\nspec:\n  kubehz:\n    access: none\n")
	mustOK(t, h.ctx.RegisterHook()(context.Background(), "none.dev", spec), h.output())
	if len(h.reqs()) != 0 {
		t.Fatal("access none must not register")
	}
	// invalid config aborts the dispatch.
	spec = h.writeSpec("bad.dev", "kind: Lo\nspec:\n  kubehz:\n    access: weird\n")
	mustErr(t, h.ctx.RegisterHook()(context.Background(), "bad.dev", spec))
	mustContain(t, h.output(), "invalid spec.kubehz.access: weird")
	// registered → the announce runs (non-fatal by contract).
	h.reset()
	h.handle("POST /api/clusters/register", 200, `{"id":"cl-1"}`)
	spec = h.writeSpec("reg.dev", "kind: Lo\nspec:\n  cluster:\n    domain: reg.dev\n  kubehz:\n    access: registered\n    apiUrl: "+h.apiURL()+"\n")
	mustOK(t, h.ctx.RegisterHook()(context.Background(), "reg.dev", spec), h.output())
	mustContain(t, h.output(), "cluster 'reg.dev' registered (pending)")
}

func TestDeregisterHookTail(t *testing.T) {
	h := newHarness(t)
	spec := h.writeSpec("reg.dev", specYAML("KubeOne", "    access: registered\n    apiUrl: "+h.apiURL()+"\n"))
	h.handle("GET /api/clusters", 200, `{"data":[{"id":"cl-1","domain":"reg.dev"}]}`)
	h.handle("DELETE /api/clusters/cl-1", 200, `{}`)
	mustOK(t, h.ctx.DeregisterHook()(context.Background(), "reg.dev", spec), h.output())
	mustContain(t, h.output(), "removed from the platform")
	none := h.writeSpec("none.dev", "kind: Lo\n")
	h.reset()
	mustOK(t, h.ctx.DeregisterHook()(context.Background(), "none.dev", none), h.output())
	if len(h.reqs()) != 0 {
		t.Fatal("access none must not deregister")
	}
}

// The driver branch tests of kubehz_hosted_test.bats: the kubeone and capi
// drivers take the hosted path through the hooks and never touch their
// self-hosted machinery.
func TestKubeoneDriverBranchesToHostedThroughHooks(t *testing.T) {
	h := newHarness(t)
	spec := h.writeSpec("test.kubehz.dev", "kind: KubeOne\nmetadata:\n  name: test\nspec:\n  kubernetes:\n    version: v1.31.0\n  kubehz:\n    hosting: hosted\n    apiUrl: "+h.apiURL()+"\n")
	h.handle("POST /api/clusters", 201, `{"data":{"id":"cl-h","status":"Running"}}`)
	h.handle("GET /api/clusters/cl-h", 200, `{"data":{"status":"Running"}}`)
	h.handle("GET /api/clusters/cl-h/kubeconfig", 200, "kc\n")
	d := kubeone.New(&driver.Deps{Paths: h.ctx.Paths, Runner: h.runner, Stderr: &h.errOut})
	d.Hooks = h.ctx.KubeoneHooks()
	mustOK(t, d.Provision(context.Background(), "test.kubehz.dev"), h.output())
	if _, err := os.Stat(filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml")); err != nil {
		t.Fatal("hosted provision did not land the kubeconfig")
	}
	if h.runner.anyCall("kubeone") {
		t.Fatal("the self-hosted machinery ran")
	}
	_ = spec

	h.reset()
	h.handle("GET /api/clusters", 200, `{"data":[{"id":"cl-h","domain":"test.kubehz.dev"}]}`)
	h.handle("DELETE /api/clusters/cl-h", 200, `{}`)
	mustOK(t, d.Destroy(context.Background(), "test.kubehz.dev"), h.output())
	if h.runner.anyCall("kubeone") {
		t.Fatal("the self-hosted teardown ran")
	}
}

func TestCapiDriverBranchesThroughHooks(t *testing.T) {
	h := newHarness(t)
	h.writeSpec("test.kubehz.dev", "kind: Capi\nmetadata:\n  name: test-cluster\nspec:\n  kubehz:\n    hosting: hosted\n    apiUrl: "+h.apiURL()+"\n")
	h.handle("POST /api/clusters", 201, `{"data":{"id":"cl-c","status":"Running"}}`)
	h.handle("GET /api/clusters/cl-c", 200, `{"data":{"status":"Running"}}`)
	h.handle("GET /api/clusters/cl-c/kubeconfig", 200, "kc\n")
	d := capi.New(&driver.Deps{Paths: h.ctx.Paths, Runner: h.runner, Stderr: &h.errOut})
	d.Hooks = h.ctx.CapiHooks()
	mustOK(t, d.Provision(context.Background(), "test.kubehz.dev"), h.output())
	if !h.anyReq("POST", "/api/clusters") {
		t.Fatal("hosted create not called")
	}

	// hosting=self with no management domain: the self-hosted advice.
	h2 := newHarness(t)
	h2.writeSpec("self.dev", "kind: Capi\nspec:\n  kubehz:\n    hosting: self\n")
	d2 := capi.New(&driver.Deps{Paths: h2.ctx.Paths, Runner: h2.runner, Stderr: &h2.errOut})
	d2.Hooks = h2.ctx.CapiHooks()
	mustErr(t, d2.Provision(context.Background(), "self.dev"))
	mustContain(t, h2.output(), "spec.managementCluster.domain is required for self-hosted CAPI")
}
