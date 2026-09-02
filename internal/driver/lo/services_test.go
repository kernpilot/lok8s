package lo

// services_test.go — CoreDNS phase behavior: the pre-metallb LAST-pool-IP
// pin, the custom-CM assembly (byte-exact generated server blocks), the
// tolerated-failure patch, and the suppressed-errexit sequencing (rollout
// restart decides the verdict).

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

func corednsFixture(t *testing.T, corednsSpec string) (*Driver, *fakeRunner, *config.Paths) {
	t.Helper()
	d, runner, _, p := testDriver(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), `apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-dns
spec:
  cluster:
    domain: test.lok8s.dev
  loadBalancer:
    pool: "10.125.50.125-10.125.50.150"
`+corednsSpec)
	corednsDir := filepath.Join(p.Lok8s, "drivers", "lo", "cluster", "coredns")
	writeFile(t, filepath.Join(corednsDir, "corefile.yaml"), "{}\n")
	writeFile(t, filepath.Join(corednsDir, "expose.yaml"), "{}\n")
	writeFile(t, filepath.Join(corednsDir, "patch.json"), "[]\n")
	return d, runner, p
}

func TestCorednsPinsExternalSvcToLastPoolIP(t *testing.T) {
	d, runner, _ := corednsFixture(t, "")
	var out, errBuf bytes.Buffer
	if err := d.coredns(context.Background(), &out, &errBuf, "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	// Pinned to the LAST pool IP so coredns-external (created pre-metallb)
	// never races the gateway for pool[0].
	want := "metallb.universe.tf/loadBalancerIPs=10.125.50.150"
	found := false
	for _, call := range runner.callsMatching("annotate svc coredns-external") {
		if strings.Contains(call, want) && strings.Contains(call, "--overwrite") {
			found = true
		}
	}
	if !found {
		t.Fatalf("LAST-pool-IP pin missing:\n%s", strings.Join(runner.calls, "\n"))
	}
}

func TestCorednsCustomHostsBlockBytes(t *testing.T) {
	d, runner, _ := corednsFixture(t, `  coredns:
    hosts:
      - name: kubehz.dev
        target: gateway
`)
	var fromFileDir string
	runner.handler = func(c execx.Cmd) error {
		joined := strings.Join(c.Args, " ")
		if c.Name == "kubectl" && strings.Contains(joined, "create configmap coredns-custom") {
			for _, a := range c.Args {
				if strings.HasPrefix(a, "--from-file=") {
					fromFileDir = strings.TrimPrefix(a, "--from-file=")
				}
			}
			// Inspect the assembled dir NOW — coredns removes it afterwards.
			got := readFileT(t, filepath.Join(fromFileDir, "host-0.server"))
			// "gateway" resolves to the FIRST pool IP; the block bytes are
			// the exact bash heredoc.
			want := `kubehz.dev:53 {
    errors
    template IN A {
        match ".*"
        answer "{{ .Name }} 30 IN A 10.125.50.125"
    }
    template IN AAAA {
        match ".*"
        rcode NOERROR
    }
    forward . /etc/resolv.conf
    cache 30
}
`
			if got != want {
				t.Errorf("generated server block diverges:\n--- want\n%s\n--- got\n%s", want, got)
			}
			writeOut(c, "apiVersion: v1\nkind: ConfigMap\n")
		}
		return nil
	}

	var out, errBuf bytes.Buffer
	if err := d.coredns(context.Background(), &out, &errBuf, "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if fromFileDir == "" {
		t.Fatal("coredns-custom ConfigMap was never assembled")
	}
	if len(runner.callsMatching("apply --kubeconfig")) == 0 {
		t.Fatal("assembled ConfigMap never applied")
	}
}

func TestCorednsPatchFailureIsToleratedRolloutDecides(t *testing.T) {
	// The patch is 2>/dev/null || true in the bash; the phase verdict is
	// the rollout restart's.
	d, runner, _ := corednsFixture(t, "")
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kubectl" && len(c.Args) > 0 && c.Args[0] == "patch" {
			return fmt.Errorf("patch refused")
		}
		return nil
	}
	var out, errBuf bytes.Buffer
	if err := d.coredns(context.Background(), &out, &errBuf, "test.lok8s.dev"); err != nil {
		t.Fatalf("a tolerated patch failure decided the phase verdict: %v", err)
	}

	// And the inverse: a failed rollout restart IS the verdict.
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kubectl" && len(c.Args) > 0 && c.Args[0] == "rollout" {
			return fmt.Errorf("rollout refused")
		}
		return nil
	}
	if err := d.coredns(context.Background(), &out, &errBuf, "test.lok8s.dev"); err == nil {
		t.Fatal("a failed rollout restart did not fail the phase")
	}
}

// ── expose ───────────────────────────────────────────────

func exposeFixture(t *testing.T) (*Driver, *fakeRunner, *config.Paths, string) {
	t.Helper()
	d, runner, _, p := testDriver(t)
	cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, cy, "spec:\n  cluster:\n    domain: test.lok8s.dev\n")
	// The REAL shipped nginx template — the envsubst whitelist + the
	// /tls.cert defect are properties of this exact file.
	writeFile(t, filepath.Join(p.Lok8s, "drivers", "lo", "cluster", "expose", "nginx.conf"),
		readFileT(t, filepath.Join(repoRoot(t), ".lok8s", "drivers", "lo", "cluster", "expose", "nginx.conf")))
	t.Setenv("KIND_EXPERIMENTAL_DOCKER_NETWORK", "lok8s")
	t.Setenv("LOK8S_LB_POOL", "10.125.50.125-10.125.50.150")
	return d, runner, p, cy
}

func TestExposeEnvsubstTwoVarWhitelist(t *testing.T) {
	d, runner, _, cy := exposeFixture(t)
	var rendered string
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "docker" && len(c.Args) >= 2 && c.Args[0] == "cp" &&
			strings.HasSuffix(c.Args[2], ":/etc/nginx/nginx.conf") {
			rendered = readFileT(t, c.Args[1])
		}
		return nil
	}

	var out, errBuf bytes.Buffer
	if err := d.expose(context.Background(), "test-dns", cy, &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if rendered == "" {
		t.Fatal("rendered nginx.conf never copied")
	}
	// The two whitelisted vars substituted…
	if !strings.Contains(rendered, "server_name *.test.lok8s.dev;") {
		t.Fatalf("LOK8S_EXPOSE_DOMAIN not substituted:\n%s", rendered)
	}
	if !strings.Contains(rendered, "proxy_pass https://10.125.50.125;") {
		t.Fatalf("LOK8S_EXPOSE_BACKEND_IP not substituted (first pool IP):\n%s", rendered)
	}
	// …and nginx's own $variables pass through untouched.
	for _, keep := range []string{"$host", "$http_upgrade", "$request_uri"} {
		if !strings.Contains(rendered, keep) {
			t.Errorf("nginx variable %s was eaten by the substitution:\n%s", keep, rendered)
		}
	}
}

func TestExposeTLSCertDefectPreserved(t *testing.T) {
	// KNOWN DEFECT preserved: the template references /tls.cert, the copy
	// lands /tls.crt. This test FAILS if either side is "fixed" in
	// isolation — the fix is a coordinated change with its own test.
	d, runner, p, cy := exposeFixture(t)
	writeFile(t, filepath.Join(p.Base, ".secrets", "tls", "tls.crt"), "CERT")
	writeFile(t, filepath.Join(p.Base, ".secrets", "tls", "tls.key"), "KEY")

	var out, errBuf bytes.Buffer
	if err := d.expose(context.Background(), "test-dns", cy, &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	copiedCrt := false
	for _, call := range runner.callsMatching("docker cp") {
		if strings.HasSuffix(call, "test-dns-proxy:/tls.crt") {
			copiedCrt = true
		}
		if strings.HasSuffix(call, "test-dns-proxy:/tls.cert") {
			t.Fatal("the copy target was 'fixed' to /tls.cert — that silently changes the proxy's TLS behavior; coordinate with the template")
		}
	}
	if !copiedCrt {
		t.Fatal("tls.crt never copied")
	}
	tmpl := readFileT(t, filepath.Join(p.Lok8s, "drivers", "lo", "cluster", "expose", "nginx.conf"))
	if !strings.Contains(tmpl, "ssl_certificate /tls.cert;") {
		t.Fatal("the shipped template no longer says /tls.cert — the preserved defect changed shape; revisit this pin")
	}
}

func TestExposeMissingTemplateFails(t *testing.T) {
	d, _, p, cy := exposeFixture(t)
	os.Remove(filepath.Join(p.Lok8s, "drivers", "lo", "cluster", "expose", "nginx.conf"))
	var out, errBuf bytes.Buffer
	if err := d.expose(context.Background(), "test-dns", cy, &out, &errBuf); err == nil {
		t.Fatal("missing template accepted")
	}
	if !strings.Contains(errBuf.String(), "expose: nginx template not found") {
		t.Fatalf("wrong error:\n%s", errBuf.String())
	}
}

// ── tunnel ───────────────────────────────────────────────

func TestKubeconfigTunnelRewritesServerEvenWhenTunnelFails(t *testing.T) {
	d, runner, _, p := testDriver(t)
	kc := filepath.Join(p.Base, ".kubeconfig", "x.yaml")
	writeFile(t, kc, `apiVersion: v1
clusters:
  - cluster:
      server: https://10.0.0.5:6443
    name: x
`)
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "ssh" {
			return fmt.Errorf("tunnel failed")
		}
		return nil
	}

	var errBuf bytes.Buffer
	if err := d.kubeconfigTunnel(context.Background(), kc, "root", "203.0.113.7", &errBuf); err != nil {
		t.Fatal(err)
	}
	// The rewrite happens REGARDLESS of tunnel success (the bash behavior).
	if !strings.Contains(readFileT(t, kc), "https://127.0.0.1:6443") {
		t.Fatalf("server not rewritten:\n%s", readFileT(t, kc))
	}
	if !strings.Contains(errBuf.String(), "SSH port-forward for API failed") {
		t.Fatalf("tunnel failure not warned:\n%s", errBuf.String())
	}
	// And the forward spec was the exact port:host:port triple.
	if len(runner.callsMatching("-L 6443:10.0.0.5:6443")) == 0 {
		t.Fatalf("forward spec wrong:\n%s", strings.Join(runner.calls, "\n"))
	}
}
