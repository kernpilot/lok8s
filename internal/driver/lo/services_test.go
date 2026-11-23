package lo

// services_test.go — CoreDNS phase behavior: the pre-metallb LAST-pool-IP
// pin, the custom-CM assembly (byte-exact generated server blocks), the
// tolerated-failure patch, and the suppressed-errexit sequencing (rollout
// restart decides the verdict).

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

func corednsFixture(t *testing.T, corednsSpec string) (*Driver, *fakeRunner, *config.Paths) {
	t.Helper()
	d, runner, _, p := testDriver(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), `apiVersion: cluster.lok8s.dev/v1beta1
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
	testutil.WriteFile(t, filepath.Join(corednsDir, "corefile.yaml"), "{}\n")
	testutil.WriteFile(t, filepath.Join(corednsDir, "expose.yaml"), "{}\n")
	testutil.WriteFile(t, filepath.Join(corednsDir, "patch.json"), "[]\n")
	return d, runner, p
}

func TestCorednsPinsExternalSvcToLastPoolIP(t *testing.T) {
	d, runner, _ := corednsFixture(t, "")
	var out, errBuf bytes.Buffer
	if err := d.coredns(t.Context(), &out, &errBuf, "test.lok8s.dev"); err != nil {
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
				if after, ok := strings.CutPrefix(a, "--from-file="); ok {
					fromFileDir = after
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
	if err := d.coredns(t.Context(), &out, &errBuf, "test.lok8s.dev"); err != nil {
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
	if err := d.coredns(t.Context(), &out, &errBuf, "test.lok8s.dev"); err != nil {
		t.Fatalf("a tolerated patch failure decided the phase verdict: %v", err)
	}

	// And the inverse: a failed rollout restart IS the verdict.
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kubectl" && len(c.Args) > 0 && c.Args[0] == "rollout" {
			return fmt.Errorf("rollout refused")
		}
		return nil
	}
	if err := d.coredns(t.Context(), &out, &errBuf, "test.lok8s.dev"); err == nil {
		t.Fatal("a failed rollout restart did not fail the phase")
	}
}

// ── expose ───────────────────────────────────────────────

func exposeFixture(t *testing.T) (*Driver, *fakeRunner, *config.Paths, string) {
	t.Helper()
	d, runner, _, p := testDriver(t)
	cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	testutil.WriteFile(t, cy, "spec:\n  cluster:\n    domain: test.lok8s.dev\n")
	// The REAL shipped nginx template: the envsubst whitelist and the TLS
	// paths are properties of this exact file.
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "drivers", "lo", "cluster", "expose", "nginx.conf"),
		readFileT(t, filepath.Join(testutil.RepoRoot(t), ".lok8s", "drivers", "lo", "cluster", "expose", "nginx.conf")))
	t.Setenv("KIND_EXPERIMENTAL_DOCKER_NETWORK", "lok8s")
	t.Setenv("LOK8S_LB_POOL", "10.125.50.125-10.125.50.150")
	// The proxy starts only with its TLS certificate and key.
	testutil.WriteFile(t, filepath.Join(p.Base, ".secrets", "tls", "tls.crt"), "CERT")
	testutil.WriteFile(t, filepath.Join(p.Base, ".secrets", "tls", "tls.key"), "KEY")
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
	if started, err := d.expose(t.Context(), "test-dns", cy, &out, &errBuf); err != nil || !started {
		t.Fatalf("expose: started %v, err %v", started, err)
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

// The two TLS copies land at the paths that nginx.conf reads: the
// ssl_certificate and ssl_certificate_key paths of the shipped template are
// the targets of the docker cp of tls.crt and of tls.key. The test reads
// both sides, from the template and from the docker argv. bats: "expose:
// the TLS copies land at the paths that nginx.conf reads"
// (tests/unit/lo_expose_test.bats).
func TestExposeTLSCopiesLandAtTheConfPaths(t *testing.T) {
	d, runner, p, cy := exposeFixture(t)
	certPath := filepath.Join(p.Base, ".secrets", "tls", "tls.crt")
	keyPath := filepath.Join(p.Base, ".secrets", "tls", "tls.key")

	var out, errBuf bytes.Buffer
	if started, err := d.expose(t.Context(), "test-dns", cy, &out, &errBuf); err != nil || !started {
		t.Fatalf("expose: started %v, err %v", started, err)
	}
	conf := readFileT(t, filepath.Join(p.Lok8s, "drivers", "lo", "cluster", "expose", "nginx.conf"))
	for _, tc := range []struct{ directive, src string }{
		{"ssl_certificate", certPath},
		{"ssl_certificate_key", keyPath},
	} {
		m := regexp.MustCompile(`(?m)^\s*`+tc.directive+`\s+(\S+);$`).FindAllStringSubmatch(conf, -1)
		if len(m) != 1 {
			t.Fatalf("nginx.conf has %d %s lines, want 1:\n%s", len(m), tc.directive, conf)
		}
		prefix := "docker cp " + tc.src + " test-dns-proxy:"
		var targets []string
		for _, call := range runner.calls {
			if target, ok := strings.CutPrefix(call, prefix); ok {
				targets = append(targets, target)
			}
		}
		if len(targets) != 1 || targets[0] != m[0][1] {
			t.Errorf("%s reads %s, but the copies of %s land at %q:\n%s", tc.directive, m[0][1], tc.src, targets, strings.Join(runner.calls, "\n"))
		}
	}
}

// Each docker step of expose must succeed. A failed step fails expose: the
// error names the step and the container, once, and the returned error is
// handled (the caller does not print it again). After the failed step,
// expose only removes the proxy container, so that no container with a
// config that is not complete stays. expose does not print "cluster
// exposed". bats: "expose: a failed docker step fails the command and
// names the step" (tests/unit/lo_expose_test.bats).
func TestExposeFailedDockerStepFailsTheCommand(t *testing.T) {
	for _, tc := range []struct{ name, call, step string }{
		{"run", "docker run ", "docker run of the proxy container test-dns-proxy"},
		{"config", " test-dns-proxy:/etc/nginx/nginx.conf", "docker cp of the nginx config to test-dns-proxy:/etc/nginx/nginx.conf"},
		{"cert", " test-dns-proxy:/tls.crt", "docker cp of the TLS certificate to test-dns-proxy:/tls.crt"},
		{"key", " test-dns-proxy:/tls.key", "docker cp of the TLS key to test-dns-proxy:/tls.key"},
		{"reload", "docker exec test-dns-proxy nginx -s reload", "docker exec test-dns-proxy nginx -s reload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, runner, _, cy := exposeFixture(t)
			failed := 0
			runner.handler = func(c execx.Cmd) error {
				if strings.Contains(c.Name+" "+strings.Join(c.Args, " "), tc.call) {
					failed++
					return gwExit{code: 3, msg: "exit status 3"}
				}
				return nil
			}
			var out, errBuf bytes.Buffer
			started, err := d.expose(t.Context(), "test-dns", cy, &out, &errBuf)
			if failed != 1 {
				t.Fatalf("the step ran %d times, want 1:\n%s", failed, strings.Join(runner.calls, "\n"))
			}
			if err == nil || started {
				t.Fatalf("expose after a failed docker step: started %v, err %v", started, err)
			}
			if !errors.Is(err, ui.ErrHandled) {
				t.Errorf("the error is not handled, so the caller prints it again: %v", err)
			}
			want := "expose: " + tc.step + " failed (exit status 3), so the proxy does not run. " +
				"Fix the cause in the docker message above, then run the command again."
			if n := strings.Count(errBuf.String(), want); n != 1 {
				t.Errorf("stderr has %d lines %q, want 1:\n%s", n, want, errBuf.String())
			}
			if strings.Contains(out.String(), "cluster exposed") {
				t.Errorf("expose printed success after a failed step:\n%s", out.String())
			}
			i := slices.IndexFunc(runner.calls, func(c string) bool { return strings.Contains(c, tc.call) })
			if after := runner.calls[i+1:]; !slices.Equal(after, []string{"docker rm -f test-dns-proxy"}) {
				t.Errorf("the calls after the failed step are %q, want only the removal of the proxy container", after)
			}
		})
	}
}

// Without the TLS certificate or key, expose does not start the proxy:
// nginx.conf serves HTTPS only. It removes the proxy of an earlier run,
// warns once and returns false with no error. It runs no other docker
// step and does not print "cluster exposed". bats: "expose: without the
// TLS certificate or key, the proxy does not start".
func TestExposeWithoutTLSFilesDoesNotStartTheProxy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove []string
	}{
		{"none", []string{"tls.crt", "tls.key"}},
		{"no key", []string{"tls.key"}},
		{"no cert", []string{"tls.crt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, runner, p, cy := exposeFixture(t)
			for _, f := range tc.remove {
				if err := os.Remove(filepath.Join(p.Base, ".secrets", "tls", f)); err != nil {
					t.Fatal(err)
				}
			}
			var out, errBuf bytes.Buffer
			started, err := d.expose(t.Context(), "test-dns", cy, &out, &errBuf)
			if err != nil || started {
				t.Fatalf("expose: started %v, err %v, want false and no error", started, err)
			}
			if got := runner.callsMatching("docker "); !slices.Equal(got, []string{"docker rm -f test-dns-proxy"}) {
				t.Errorf("docker calls = %q, want only the removal of the proxy container", got)
			}
			want := "expose: no TLS certificate and key at " + filepath.Join(p.Base, ".secrets", "tls", "tls.crt") +
				" and " + filepath.Join(p.Base, ".secrets", "tls", "tls.key") + ", so the proxy is not started. " +
				"Add both files, or set spec.remote.expose: false."
			if n := strings.Count(errBuf.String(), want); n != 1 {
				t.Errorf("stderr has %d warnings %q, want 1:\n%s", n, want, errBuf.String())
			}
			if strings.Contains(out.String(), "cluster exposed") {
				t.Errorf("expose printed success with no proxy:\n%s", out.String())
			}
		})
	}
}

// The docker steps of expose run in this order: the removal of the proxy
// of an earlier run, the run, the copies of the config, the certificate
// and the key, and the reload. bats: "expose: the docker steps run in
// order".
func TestExposeRunsTheStepsInOrder(t *testing.T) {
	d, runner, p, cy := exposeFixture(t)
	var out, errBuf bytes.Buffer
	if started, err := d.expose(t.Context(), "test-dns", cy, &out, &errBuf); err != nil || !started {
		t.Fatalf("expose: started %v, err %v", started, err)
	}
	conf := regexp.MustCompile(`^docker cp \S*lok8s-nginx\.[^ ]*\.conf `)
	var got []string
	for _, c := range runner.callsMatching("docker ") {
		got = append(got, conf.ReplaceAllString(c, "docker cp <conf> "))
	}
	want := []string{
		"docker rm -f test-dns-proxy",
		"docker run -d --restart=always --name test-dns-proxy --network lok8s -p 80:80 -p 443:443 " + ExposeImage,
		"docker cp <conf> test-dns-proxy:/etc/nginx/nginx.conf",
		"docker cp " + filepath.Join(p.Base, ".secrets", "tls", "tls.crt") + " test-dns-proxy:/tls.crt",
		"docker cp " + filepath.Join(p.Base, ".secrets", "tls", "tls.key") + " test-dns-proxy:/tls.key",
		"docker exec test-dns-proxy nginx -s reload",
	}
	if !slices.Equal(got, want) {
		t.Errorf("docker calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(out.String(), ":: cluster exposed at https://*.test.lok8s.dev") {
		t.Errorf("no success line:\n%s", out.String())
	}
}

// The proxy container runs the pinned nginx image: the tag of one version
// and its digest, never a floating tag. The literal is the value of
// ExposeImage and of LO_EXPOSE_IMAGE (the bash twin). No other docker call
// names an nginx image, so a pull of another tag fails the test too. bats:
// "expose: the proxy runs the pinned nginx image, by tag and digest"
// (tests/unit/lo_expose_test.bats).
func TestExposeRunsThePinnedImage(t *testing.T) {
	d, runner, _, cy := exposeFixture(t)
	var out, errBuf bytes.Buffer
	if started, err := d.expose(t.Context(), "test-dns", cy, &out, &errBuf); err != nil || !started {
		t.Fatalf("expose: started %v, err %v", started, err)
	}
	const want = "docker run -d --restart=always --name test-dns-proxy --network lok8s -p 80:80 -p 443:443 " +
		"nginx:1.30.7-alpine3.24@sha256:caf4e1e032a20f9c3c5e28fc7f25ffd768ad8677b4975c54193075997b3a97e3"
	if got := runner.callsMatching("docker run"); len(got) != 1 || got[0] != want {
		t.Fatalf("docker run calls = %q, want exactly one: %s", got, want)
	}
	if got := dockerCallsNamingNginx(runner.calls); len(got) != 1 || got[0] != want {
		t.Fatalf("docker calls that name an nginx image = %q, want only: %s", got, want)
	}
}

// nginxImageRe matches a word that names an nginx image: nginx, or a
// registry path that ends in /nginx, with an optional tag or digest. A path
// in a container (test-dns-proxy:/etc/nginx/nginx.conf) does not match.
var nginxImageRe = regexp.MustCompile(`^(\S*/)?nginx([:@]\S*)?$`)

// dockerCallsNamingNginx returns each docker call that has a word that names
// an nginx image. A docker exec names a container, not an image: its nginx
// is the command in the container (nginx -s reload). The bash twin is the
// same loop in tests/unit/lo_expose_test.bats.
func dockerCallsNamingNginx(calls []string) []string {
	var named []string
	for _, c := range calls {
		w := strings.Fields(c)
		if len(w) < 2 || w[0] != "docker" || w[1] == "exec" {
			continue
		}
		if slices.ContainsFunc(w[1:], nginxImageRe.MatchString) {
			named = append(named, c)
		}
	}
	return named
}

// The two twins pin the same image: the value of LO_EXPOSE_IMAGE in the
// bash defaults is ExposeImage, byte for byte. A bump of one twin only
// fails here.
func TestExposeImageMatchesTheBashDefault(t *testing.T) {
	src := readFileT(t, filepath.Join(testutil.RepoRoot(t), ".lok8s", "drivers", "lo", "utils", "defaults.sh"))
	m := regexp.MustCompile(`(?m)^LO_EXPOSE_IMAGE="([^"]*)"$`).FindAllStringSubmatch(src, -1)
	if len(m) != 1 {
		t.Fatalf("defaults.sh has %d lines LO_EXPOSE_IMAGE=\"…\", want exactly 1", len(m))
	}
	if m[0][1] != ExposeImage {
		t.Errorf("the twins differ: LO_EXPOSE_IMAGE in defaults.sh is %q, ExposeImage is %q", m[0][1], ExposeImage)
	}
}

func TestExposeMissingTemplateFails(t *testing.T) {
	d, _, p, cy := exposeFixture(t)
	os.Remove(filepath.Join(p.Lok8s, "drivers", "lo", "cluster", "expose", "nginx.conf"))
	var out, errBuf bytes.Buffer
	if _, err := d.expose(t.Context(), "test-dns", cy, &out, &errBuf); err == nil {
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
	testutil.WriteFile(t, kc, `apiVersion: v1
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
	if err := d.kubeconfigTunnel(t.Context(), kc, "root", "203.0.113.7", &errBuf); err != nil {
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
