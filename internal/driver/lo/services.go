package lo

// services.go — cluster service setup: CoreDNS
// (.lok8s/drivers/lo/utils/services.sh).
//
// ERREXIT-SUPPRESSION PARITY NOTE: the bash lo::coredns always ran under a
// caller's `|| return 1` guard, which disables errexit for the whole
// function body — so an individual failed kubectl step did NOT abort the
// sequence, and the function's status was the LAST command's (the rollout
// restart). The port mirrors that: every step runs, intermediate errors are
// not fatal, and the rollout restart's error is the returned one. Do not
// "fix" this into fail-fast without renegotiating the phase's contract.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// coredns applies the CoreDNS base config, service pin, custom ConfigMap,
// deployment patch and rollout restart (bash: lo::coredns).
func (d *Driver) coredns(ctx context.Context, out, errOut io.Writer, domain string) error {
	corednsDir := filepath.Join(d.deps.Paths.Lok8s, "drivers", "lo", "cluster", "coredns")
	clusterYAML := filepath.Join(d.deps.Paths.Clusters, domain, "cluster.lok8s.yaml")
	root := loadYAML(clusterYAML)
	clusterName := yqRaw(root, "metadata", "name")
	if clusterName == "null" {
		clusterName = ""
	}
	kubeconfig := filepath.Join(d.deps.Paths.Base, ".kubeconfig", clusterName+".yaml")

	_ = d.runOut(ctx, out, errOut, "kubectl", "apply", "--kubeconfig", kubeconfig, "-f", filepath.Join(corednsDir, "corefile.yaml"))
	_ = d.runOut(ctx, out, errOut, "kubectl", "apply", "--kubeconfig", kubeconfig, "-f", filepath.Join(corednsDir, "expose.yaml"))

	// Pin coredns-external to the LAST loadBalancer.pool IP so it does not
	// race the ingress/Envoy gateway for pool[0]. coredns-external is
	// created HERE — before the metallb bootstrap addon — so without a pin
	// metallb later hands it the first free pool IP (pool[0]); a gateway
	// pinned to pool[0] (the convention, and what spec.coredns
	// `target: gateway` resolves to) then cannot allocate it ("address
	// already in use by coredns-external") → gateway stuck <pending> →
	// nothing serves. Setting the annotation now (pre-metallb) makes metallb
	// honor it on first allocation. Only meaningful for a range pool.
	pool := yqOr(root, "", "spec", "loadBalancer", "pool")
	if strings.Contains(pool, "-") {
		last := pool[strings.LastIndex(pool, "-")+1:]
		_ = d.runOut(ctx, out, errOut, "kubectl", "annotate", "svc", "coredns-external", "-n", "kube-system",
			"--kubeconfig", kubeconfig,
			"metallb.universe.tf/loadBalancerIPs="+last, "--overwrite")
	}

	// Per-cluster custom CoreDNS from spec.coredns — loaded into the
	// `coredns-custom` ConfigMap, imported by the Corefile from
	// /etc/coredns/custom (see corefile.yaml + patch.json). Declarative +
	// committed, survives `lo up`.
	d.corednsCustom(ctx, out, errOut, domain, clusterYAML, kubeconfig)

	// Tolerated-failure patch (bash: 2>/dev/null || true).
	_ = d.runQuiet(ctx, "kubectl", "patch", "deployment", "coredns", "-n", "kube-system",
		"--kubeconfig", kubeconfig, "--type", "json",
		"--patch-file", filepath.Join(corednsDir, "patch.json"))

	return d.runOut(ctx, out, errOut, "kubectl", "rollout", "restart", "deployment/coredns",
		"-n", "kube-system", "--kubeconfig", kubeconfig)
}

// corednsCustom builds the coredns-custom ConfigMap from spec.coredns
// (bash: lo::coredns_custom). All three inputs compose (and are optional);
// nothing configured → no ConfigMap (the Corefile's `import custom/*` is
// then a no-op):
//
//	spec.coredns.hosts[]   {name,target} → a generated server block resolving
//	                       the zone `name` (apex + every *.name) to `target`:
//	                       A → target, AAAA → NODATA (no dual-stack
//	                       SERVFAIL), other types forwarded. target
//	                       "gateway" resolves to the first
//	                       spec.loadBalancer.pool IP (where the gateway pins
//	                       by convention) — so the IP isn't duplicated by hand.
//	spec.coredns.servers   raw CoreDNS server block(s), inline (a *.server file)
//	spec.coredns.overrides raw directives merged into the default .:53 block
//	spec.coredns.import    path (relative to the cluster dir; default
//	                       ./coredns) to a dir of raw *.server / *.override files
//
// Do NOT define the same zone via both hosts and a raw server/import
// (CoreDNS rejects duplicate zone blocks).
func (d *Driver) corednsCustom(ctx context.Context, out, errOut io.Writer, domain, clusterYAML, kubeconfig string) {
	tmp, err := os.MkdirTemp("", "lok8s-coredns-")
	if err != nil {
		return
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	root := loadYAML(clusterYAML)

	// gateway shorthand = first IP of the LB pool ("a-b" → "a").
	pool := yqOr(root, "", "spec", "loadBalancer", "pool")
	gatewayIP := pool
	if i := strings.Index(pool, "-"); i >= 0 {
		gatewayIP = pool[:i]
	}

	// (1) structured hosts → generated server blocks.
	for i, h := range yqSeq(root, "spec", "coredns", "hosts") {
		name := yqRaw(h, "name")
		target := yqRaw(h, "target")
		if target == "gateway" {
			target = gatewayIP
		}
		block := fmt.Sprintf(`%s:53 {
    errors
    template IN A {
        match ".*"
        answer "{{ .Name }} 30 IN A %s"
    }
    template IN AAAA {
        match ".*"
        rcode NOERROR
    }
    forward . /etc/resolv.conf
    cache 30
}
`, name, target)
		_ = os.WriteFile(filepath.Join(tmp, fmt.Sprintf("host-%d.server", i)), []byte(block), 0o644)
	}

	// (2) raw inline servers / overrides.
	if servers := yqOr(root, "", "spec", "coredns", "servers"); servers != "" {
		_ = os.WriteFile(filepath.Join(tmp, "inline.server"), []byte(servers+"\n"), 0o644)
	}
	if overrides := yqOr(root, "", "spec", "coredns", "overrides"); overrides != "" {
		_ = os.WriteFile(filepath.Join(tmp, "inline.override"), []byte(overrides+"\n"), 0o644)
	}

	// (3) raw files from the import path (default ./coredns, relative to
	// the cluster dir).
	importPath := yqOr(root, "./coredns", "spec", "coredns", "import")
	if !strings.HasPrefix(importPath, "/") {
		importPath = filepath.Join(d.deps.Paths.Clusters, domain, strings.TrimPrefix(importPath, "./"))
	}
	if info, err := os.Stat(importPath); err == nil && info.IsDir() {
		for _, glob := range []string{"*.server", "*.override"} {
			matches, _ := filepath.Glob(filepath.Join(importPath, glob))
			for _, m := range matches {
				if raw, err := os.ReadFile(m); err == nil {
					_ = os.WriteFile(filepath.Join(tmp, filepath.Base(m)), raw, 0o644)
				}
			}
		}
	}

	entries, _ := os.ReadDir(tmp)
	if len(entries) == 0 {
		return
	}

	// kubectl create configmap … --dry-run=client -o yaml | kubectl apply -f -
	rendered, err := d.output(ctx, "kubectl", "create", "configmap", "coredns-custom",
		"-n", "kube-system", "--kubeconfig", kubeconfig,
		"--from-file="+tmp, "--dry-run=client", "-o", "yaml")
	if err != nil {
		return
	}
	_ = d.runInput(ctx, rendered+"\n", out, errOut, "kubectl", "apply", "--kubeconfig", kubeconfig, "-f", "-")
}

// runOut runs a tool with the caller's writers (progress phases capture
// both).
func (d *Driver) runOut(ctx context.Context, out, errOut io.Writer, name string, args ...string) error {
	return d.deps.Runner.Run(ctx, cmdWith(name, args, out, errOut))
}
