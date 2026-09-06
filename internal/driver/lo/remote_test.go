package lo

// remote_test.go — the remote-VM waits (exact budgets + messages) and the
// remote-CI command lines (env-rewritten ssh commands, summary block) from
// .lok8s/drivers/lo/utils/remote.sh.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

func TestProvisionRemoteNoNodesFallsBackWithWarning(t *testing.T) {
	d, _, errBuf, _ := testDriver(t)
	d.deps.Provider = &fakeLoProvider{output: []byte(`{"nodes":[]}`)}
	d.deps.ProviderName = "hetzner"

	if err := d.provisionRemote(context.Background(), "test.lok8s.dev", "spec.yaml", errBuf); err != nil {
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

	err := d.provisionRemote(context.Background(), "test.lok8s.dev", "spec.yaml", errBuf)
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

	err := d.provisionRemote(context.Background(), "test.lok8s.dev", "spec.yaml", errBuf)
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

	if err := d.provisionRemote(context.Background(), "test.lok8s.dev", "spec.yaml", errBuf); err != nil {
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

func TestRemoteCICommandLinesAndSummary(t *testing.T) {
	d, runner, _, p := testDriver(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), specShared)
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
	if err := d.remoteCI(context.Background(), "test.lok8s.dev", d.clusterYAML("test.lok8s.dev"), &out, &errBuf); err != nil {
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
