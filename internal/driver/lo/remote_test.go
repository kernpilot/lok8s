package lo

// remote_test.go — the remote-VM waits (exact budgets + messages) and the
// remote-CI command lines (env-rewritten ssh commands, summary block) from
// .lok8s/drivers/lo/utils/remote.sh.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/provider/bridge"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// A failed provider output stops the remote provision: the provider's
// message (its stderr, through the real bridge) shows above the error line,
// and the provision does not fall back to a local kind.
func TestProvisionRemoteFailedOutputShowsTheProviderMessage(t *testing.T) {
	d, _, errBuf, p := testDriver(t)
	stub := &stubProviderRunner{fn: "provider::output", stderr: "hetzner: hcloud server list failed: unable to authenticate (unauthorized)\n"}
	prov, err := (&bridge.Loader{Paths: p, Runner: stub, Stdout: io.Discard, Stderr: errBuf}).Load(t.Context(), "stub")
	if err != nil {
		t.Fatal(err)
	}
	d.deps.ProviderName, d.deps.Provider = "stub", prov

	if err := d.provisionRemote(t.Context(), "test.lok8s.dev", "spec.yaml", errBuf); err == nil {
		t.Fatal("a failed provider output reported success")
	}
	want := "hetzner: hcloud server list failed: unable to authenticate (unauthorized)\n" +
		"[error] provider output failed — refusing to treat it as 'no nodes'\n"
	if !strings.Contains(errBuf.String(), want) {
		t.Fatalf("the provider's message is not above the error line:\n%s", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "running kind locally") {
		t.Fatalf("the provision fell back to a local kind:\n%s", errBuf.String())
	}
}

func TestProvisionRemoteNoNodesFallsBackWithWarning(t *testing.T) {
	d, _, errBuf, _ := testDriver(t)
	d.deps.Provider = &fakeLoProvider{output: []byte(`{"nodes":[]}`)}
	d.deps.ProviderName = "hetzner"

	if err := d.provisionRemote(t.Context(), "test.lok8s.dev", "spec.yaml", errBuf); err != nil {
		t.Fatalf("no-nodes fallback errored: %v", err)
	}
	if !strings.Contains(errBuf.String(), "provider loaded but no nodes in output — running kind locally") {
		t.Fatalf("fallback warning missing:\n%s", errBuf.String())
	}
	if os.Getenv("DOCKER_HOST") != "" {
		t.Fatal("DOCKER_HOST set on the local fallback")
	}
}

func TestProvisionRemoteSSHNeverComesUp(t *testing.T) {
	d, runner, errBuf, _ := testDriver(t)
	d.deps.Provider = &fakeLoProvider{output: []byte(`{"nodes":[{"public_ip":"203.0.113.7","ssh_user":"root"}]}`)}
	d.deps.ProviderName = "hetzner"

	runner.handler = func(c execx.Cmd) error {
		if c.Name == "ssh" {
			return fmt.Errorf("connection refused")
		}
		return nil
	}

	err := d.provisionRemote(t.Context(), "test.lok8s.dev", "spec.yaml", errBuf)
	if err == nil {
		t.Fatal("unreachable SSH reported success")
	}
	// Exact budget message: 30 attempts × 2s.
	if !strings.Contains(errBuf.String(), "SSH not reachable on 203.0.113.7 after 60s") {
		t.Fatalf("budget message wrong:\n%s", errBuf.String())
	}
	if got := len(runner.callsMatching("ssh -o ConnectTimeout=5")); got != 30 {
		t.Fatalf("ssh attempts = %d, want 30", got)
	}
}

func TestProvisionRemoteDockerNeverComesUp(t *testing.T) {
	d, runner, errBuf, _ := testDriver(t)
	d.deps.Provider = &fakeLoProvider{output: []byte(`{"nodes":[{"public_ip":"203.0.113.7"}]}`)}
	d.deps.ProviderName = "hetzner"

	runner.handler = func(c execx.Cmd) error {
		if c.Name == "ssh" {
			joined := strings.Join(c.Args, " ")
			if strings.Contains(joined, "docker info") {
				return fmt.Errorf("no docker")
			}
			return nil // ssh + cloud-init succeed
		}
		return nil
	}

	err := d.provisionRemote(t.Context(), "test.lok8s.dev", "spec.yaml", errBuf)
	if err == nil {
		t.Fatal("missing docker reported success")
	}
	// Exact message incl. the log hint + the defaulted root user.
	if !strings.Contains(errBuf.String(),
		"Docker not available on 203.0.113.7 after 180s. Check cloud-init logs: ssh root@203.0.113.7 cat /var/log/cloud-init-output.log") {
		t.Fatalf("docker budget message wrong:\n%s", errBuf.String())
	}
}

func TestProvisionRemoteHappyPathSetsDockerHost(t *testing.T) {
	d, _, errBuf, _ := testDriver(t)
	d.deps.Provider = &fakeLoProvider{output: []byte(`{"nodes":[{"public_ip":"203.0.113.7","ssh_user":"ci"}]}`)}
	d.deps.ProviderName = "hetzner"

	if err := d.provisionRemote(t.Context(), "test.lok8s.dev", "spec.yaml", errBuf); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOK8S_REMOTE_IP") != "203.0.113.7" || os.Getenv("LOK8S_REMOTE_USER") != "ci" {
		t.Fatal("LOK8S_REMOTE_* not exported")
	}
	if os.Getenv("DOCKER_HOST") != "ssh://ci@203.0.113.7" {
		t.Fatalf("DOCKER_HOST = %q", os.Getenv("DOCKER_HOST"))
	}
	t.Cleanup(func() { os.Unsetenv("DOCKER_HOST") })
}

// remoteCIExposeFixture is a remote CI run with the expose on: the shipped
// nginx template, and a scp that writes the kubeconfig of the VM, so that
// the tunnel step runs. dockerHosts records DOCKER_HOST for each docker
// call.
func remoteCIExposeFixture(t *testing.T, failDocker string) (*Driver, *fakeRunner, *config.Paths, *[]string) {
	t.Helper()
	d, runner, _, p := testDriver(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), specShared)
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "drivers", "lo", "cluster", "expose", "nginx.conf"),
		readFileT(t, filepath.Join(testutil.RepoRoot(t), ".lok8s", "drivers", "lo", "cluster", "expose", "nginx.conf")))
	t.Setenv("LOK8S_REMOTE_IP", "203.0.113.7")
	t.Setenv("LOK8S_REMOTE_USER", "root")
	t.Setenv("LOK8S_REMOTE_SYNC_DEST", "/workspace")
	t.Setenv("LOK8S_REMOTE_SYNC_PATH", ".")
	t.Setenv("LOK8S_REMOTE_SYNC_EXCLUDE", ".git")
	t.Setenv("LOK8S_REMOTE_TILT", "false")
	t.Setenv("LOK8S_REMOTE_EXPOSE", "true")
	t.Setenv("DOCKER_HOST", "unix:///before.sock")
	var dockerHosts []string
	runner.handler = func(c execx.Cmd) error {
		switch c.Name {
		case "git":
			writeOut(c, p.Base+"\n")
		case "scp":
			testutil.WriteFile(t, c.Args[len(c.Args)-1],
				"apiVersion: v1\nclusters:\n- cluster:\n    server: https://10.0.0.1:6443\n  name: vm\n")
		case "docker":
			dockerHosts = append(dockerHosts, os.Getenv("DOCKER_HOST"))
			if len(c.Args) > 0 && c.Args[0] == failDocker {
				return gwExit{code: 3, msg: "exit status 3"}
			}
		}
		return nil
	}
	return d, runner, p, &dockerHosts
}

// A failed expose does not stop the remote CI run before the kubeconfig
// fetch and the tunnel: the cluster runs. The run fails at the end, and
// the summary shows no proxy URL. The expose runs with DOCKER_HOST set to
// the VM, and DOCKER_HOST is back to its value from before. bats: "remote
// CI: a failed expose still sets up the kubeconfig, then fails the run"
// (tests/unit/lo_expose_test.bats).
func TestRemoteCIFailedExposeFailsTheRun(t *testing.T) {
	d, runner, p, dockerHosts := remoteCIExposeFixture(t, "run")
	testutil.WriteFile(t, filepath.Join(p.Base, ".secrets", "tls", "tls.crt"), "CERT")
	testutil.WriteFile(t, filepath.Join(p.Base, ".secrets", "tls", "tls.key"), "KEY")

	var out, errBuf bytes.Buffer
	if err := d.remoteCI(t.Context(), "test.lok8s.dev", d.clusterYAML("test.lok8s.dev"), &out, &errBuf); err == nil {
		t.Fatalf("remoteCI succeeded after a failed expose:\n%s", out.String())
	}
	if want := "expose: docker run of the proxy container test-lifecycle-proxy failed (exit status 3)"; !strings.Contains(errBuf.String(), want) {
		t.Errorf("stderr does not name the failed step, want %q:\n%s", want, errBuf.String())
	}
	if len(*dockerHosts) == 0 {
		t.Fatal("expose ran no docker call")
	}
	for _, h := range *dockerHosts {
		if h != "ssh://root@203.0.113.7" {
			t.Errorf("a docker call of the expose ran with DOCKER_HOST %q, want ssh://root@203.0.113.7", h)
		}
	}
	if got := runner.callsMatching("scp root@203.0.113.7:/workspace/.kubeconfig/test-lifecycle.yaml"); len(got) != 1 {
		t.Errorf("the kubeconfig fetch ran %d times, want 1:\n%s", len(got), strings.Join(runner.calls, "\n"))
	}
	if got := runner.callsMatching("ssh -fN"); len(got) != 1 {
		t.Errorf("the API tunnel ran %d times, want 1:\n%s", len(got), strings.Join(runner.calls, "\n"))
	}
	if !strings.Contains(out.String(), ":: remote CI cluster ready") || strings.Contains(out.String(), "URL:") {
		t.Errorf("want the summary with no proxy URL:\n%s", out.String())
	}
	if want := "remote CI: the cluster runs, but expose failed (its error is above), so the run fails."; !strings.Contains(errBuf.String(), want) {
		t.Errorf("no final error line %q:\n%s", want, errBuf.String())
	}
	if got := os.Getenv("DOCKER_HOST"); got != "unix:///before.sock" {
		t.Errorf("DOCKER_HOST = %q after the failed expose, want the value from before", got)
	}
}

// Without the TLS files, the expose of a remote CI run starts no proxy and
// the run succeeds, but the summary shows no proxy URL. With the files, it
// shows the URL. bats: "remote CI: the summary shows the proxy URL only
// when the proxy runs".
func TestRemoteCISummaryShowsTheProxyURLOnlyWhenTheProxyRuns(t *testing.T) {
	for _, tls := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%v", tls), func(t *testing.T) {
			d, _, p, _ := remoteCIExposeFixture(t, "")
			if tls {
				testutil.WriteFile(t, filepath.Join(p.Base, ".secrets", "tls", "tls.crt"), "CERT")
				testutil.WriteFile(t, filepath.Join(p.Base, ".secrets", "tls", "tls.key"), "KEY")
			}
			var out, errBuf bytes.Buffer
			if err := d.remoteCI(t.Context(), "test.lok8s.dev", d.clusterYAML("test.lok8s.dev"), &out, &errBuf); err != nil {
				t.Fatalf("remoteCI: %v\n%s", err, errBuf.String())
			}
			if got := strings.Contains(out.String(), "   URL:        https://*.test.lok8s.dev (via 203.0.113.7:443)"); got != tls {
				t.Errorf("URL line in the summary = %v, want %v:\n%s", got, tls, out.String())
			}
		})
	}
}

func TestRemoteCICommandLinesAndSummary(t *testing.T) {
	d, runner, _, p := testDriver(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), specShared)
	t.Setenv("LOK8S_REMOTE_IP", "203.0.113.7")
	t.Setenv("LOK8S_REMOTE_USER", "root")
	t.Setenv("LOK8S_REMOTE_SYNC_DEST", "/workspace")
	t.Setenv("LOK8S_REMOTE_SYNC_PATH", ".")
	t.Setenv("LOK8S_REMOTE_SYNC_EXCLUDE", ".git\nnode_modules\n.secrets\n.kubeconfig\nclusters/.active")
	t.Setenv("LOK8S_REMOTE_TILT", "true")
	t.Setenv("LOK8S_REMOTE_EXPOSE", "false")

	runner.handler = func(c execx.Cmd) error {
		if c.Name == "git" {
			writeOut(c, p.Base+"\n")
		}
		return nil
	}

	var out, errBuf bytes.Buffer
	if err := d.remoteCI(t.Context(), "test.lok8s.dev", d.clusterYAML("test.lok8s.dev"), &out, &errBuf); err != nil {
		t.Fatalf("remoteCI: %v\n%s", err, errBuf.String())
	}

	// The env-rewritten remote provision command — the exact line-continued
	// string the bash composed (continuation indentation preserved).
	wantProvision := "ssh root@203.0.113.7 cd '/workspace' &&     export DOMAIN_NAME='test.lok8s.dev' &&     export PATH_BASE='/workspace' &&     export PATH_LOK8S='/workspace/.lok8s' &&     export PATH_CLUSTERS='/workspace/clusters' &&     export PATH_BIN='/workspace/.bin' &&     export KUSTOMIZE_PLUGIN_HOME='/workspace/.kustomize' &&     export PATH=\"/workspace/.lok8s:/workspace/.bin:${PATH}\" &&     .lok8s/lo provision --domain 'test.lok8s.dev'"
	found := false
	for _, call := range runner.calls {
		if call == wantProvision {
			found = true
		}
	}
	if !found {
		t.Fatalf("remote provision command diverges.\nwant: %s\ngot calls:\n%s", wantProvision, strings.Join(runner.callsMatching("ssh"), "\n"))
	}

	// rsync with the default excludes + trailing-slash source.
	rsyncCalls := runner.callsMatching("rsync -az --delete --info=progress2")
	if len(rsyncCalls) == 0 {
		t.Fatalf("main rsync missing:\n%s", strings.Join(runner.calls, "\n"))
	}
	for _, excl := range []string{"--exclude=.git", "--exclude=.secrets", "--exclude=clusters/.active"} {
		if !strings.Contains(rsyncCalls[0], excl) {
			t.Errorf("rsync missing %s: %s", excl, rsyncCalls[0])
		}
	}
	if !strings.Contains(rsyncCalls[0], p.Base+"/./ root@203.0.113.7:/workspace/") {
		t.Errorf("rsync src/dest wrong: %s", rsyncCalls[0])
	}

	// The tilt nohup command.
	wantTilt := "ssh root@203.0.113.7 cd '/workspace' &&     export DOMAIN_NAME='test.lok8s.dev' &&     export PATH_BASE='/workspace' &&     export PATH_LOK8S='/workspace/.lok8s' &&     export PATH_CLUSTERS='/workspace/clusters' &&     nohup .lok8s/lo tilt up > /tmp/lok8s-tilt.log 2>&1 &"
	found = false
	for _, call := range runner.calls {
		if call == wantTilt {
			found = true
		}
	}
	if !found {
		t.Fatalf("remote tilt command diverges.\nwant: %s\ngot:\n%s", wantTilt, strings.Join(runner.callsMatching("tilt"), "\n"))
	}

	// Summary block (exact lines).
	summary := out.String()
	for _, want := range []string{
		":: remote CI cluster ready",
		"   VM:         203.0.113.7",
		"   SSH:        ssh root@203.0.113.7",
		"   kubectl:    KUBECONFIG=" + filepath.Join(p.Base, ".kubeconfig", "test-lifecycle.yaml") + " kubectl get nodes",
		"   Tilt log:   ssh root@203.0.113.7 tail -f /tmp/lok8s-tilt.log",
		"   Sync:       rsync -az " + p.Base + "/./ root@203.0.113.7:/workspace/",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
	// EXPOSE off → no URL line.
	if strings.Contains(summary, "   URL:") {
		t.Errorf("URL line printed with expose disabled:\n%s", summary)
	}
}
