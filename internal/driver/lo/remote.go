package lo

// remote.go — remote VM provisioning + CI mode
// (.lok8s/drivers/lo/utils/remote.sh).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/ui"
)

// provisionRemote waits for SSH, cloud-init, and Docker on a freshly
// provisioned VM (bash: lo::provision_remote). Sets LOK8S_REMOTE_IP,
// LOK8S_REMOTE_USER and DOCKER_HOST (process env — the bash exported them;
// every later docker invocation rides DOCKER_HOST).
func (d *Driver) provisionRemote(ctx context.Context, domain, clusterYAML string, errOut io.Writer) error {
	workDir := filepath.Join(d.deps.Paths.Clusters, domain, ".provider")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}

	// Guarded like the call sites in drivers/lo/main: a FAILED provider
	// (API error, quota, bad credentials) must surface as an error, not
	// conflate with the legitimate "loaded but no nodes" fallback below —
	// under the dispatch's suppressed-errexit equivalent, an unguarded call
	// would fall through with empty output and masquerade as no-nodes.
	if err := d.deps.Provider.Provision(ctx, d.deps.ProviderConfigFile, workDir); err != nil {
		ui.Errorf(errOut, "provider provision failed — refusing to treat it as 'no nodes'")
		return fmt.Errorf("provider provision failed: %w", err)
	}

	providerOutput, err := d.deps.Provider.Output(ctx, d.deps.ProviderConfigFile)
	if err != nil {
		ui.Errorf(errOut, "provider output failed — refusing to treat it as 'no nodes'")
		return fmt.Errorf("provider output failed: %w", err)
	}

	remoteIP, remoteUser := providerNode0(providerOutput)

	if remoteIP == "" {
		ui.Warnf(errOut, "provider loaded but no nodes in output — running kind locally")
		return nil
	}

	// Wait for SSH: 30 × 2s.
	ui.Debugf(errOut, "waiting for SSH on %s...", remoteIP)
	sshOK := false
	for attempts := 0; attempts < 30; attempts++ {
		if d.runQuiet(ctx, "ssh", "-o", "ConnectTimeout=5", "-o", "BatchMode=yes",
			remoteUser+"@"+remoteIP, "true") == nil {
			sshOK = true
			ui.Debugf(errOut, "SSH ready on %s (after %ds)", remoteIP, attempts*2)
			break
		}
		d.sleepSeconds(2)
	}
	if !sshOK {
		ui.Errorf(errOut, "SSH not reachable on %s after 60s", remoteIP)
		return fmt.Errorf("ssh not reachable on %s", remoteIP)
	}

	// Wait for cloud-init: 90 × 3s — a timeout WARNS and proceeds (cloud-init
	// completion is best-effort; Docker readiness below is the hard gate).
	ui.Debugf(errOut, "waiting for cloud-init to finish on %s...", remoteIP)
	ciDone := false
	for attempts := 0; attempts < 90; attempts++ {
		if d.runQuiet(ctx, "ssh", remoteUser+"@"+remoteIP,
			"test -f /var/lib/cloud/instance/boot-finished") == nil {
			ciDone = true
			ui.Debugf(errOut, "cloud-init finished on %s (after %ds)", remoteIP, attempts*3)
			break
		}
		d.sleepSeconds(3)
	}
	if !ciDone {
		ui.Warnf(errOut, "cloud-init did not finish within 270s — proceeding anyway")
	}

	// Wait for Docker: 60 × 3s.
	ui.Debugf(errOut, "waiting for Docker on %s...", remoteIP)
	dockerOK := false
	for attempts := 0; attempts < 60; attempts++ {
		if d.runQuiet(ctx, "ssh", remoteUser+"@"+remoteIP, "command -v docker && docker info") == nil {
			dockerOK = true
			ui.Debugf(errOut, "Docker ready on %s (after %ds)", remoteIP, attempts*3)
			break
		}
		d.sleepSeconds(3)
	}
	if !dockerOK {
		ui.Errorf(errOut, "Docker not available on %s after 180s. Check cloud-init logs: ssh %s@%s cat /var/log/cloud-init-output.log", remoteIP, remoteUser, remoteIP)
		return fmt.Errorf("docker not available on %s", remoteIP)
	}

	os.Setenv("LOK8S_REMOTE_IP", remoteIP)
	os.Setenv("LOK8S_REMOTE_USER", remoteUser)

	os.Setenv("DOCKER_HOST", "ssh://"+remoteUser+"@"+remoteIP)
	ui.Debugf(errOut, "remote Docker: DOCKER_HOST=%s", os.Getenv("DOCKER_HOST"))

	// Verify Docker is reachable via DOCKER_HOST: 10 × 3s.
	for attempts := 0; attempts < 10; attempts++ {
		if d.runQuiet(ctx, "docker", "info") == nil {
			ui.Debugf(errOut, "DOCKER_HOST verified (attempt %d)", attempts)
			return nil
		}
		d.sleepSeconds(3)
	}
	ui.Errorf(errOut, "Docker not reachable via DOCKER_HOST=%s", os.Getenv("DOCKER_HOST"))
	return fmt.Errorf("docker not reachable via DOCKER_HOST")
}

// providerNode0 extracts nodes[0].{public_ip,ssh_user} from the provider's
// inventory JSON (bash: the jq reads, ssh_user defaulting to root).
func providerNode0(out []byte) (ip, user string) {
	var doc struct {
		Nodes []struct {
			PublicIP string `json:"public_ip"`
			SSHUser  string `json:"ssh_user"`
		} `json:"nodes"`
	}
	user = "root"
	if json.Unmarshal(out, &doc) != nil || len(doc.Nodes) == 0 {
		return "", user
	}
	ip = doc.Nodes[0].PublicIP
	if doc.Nodes[0].SSHUser != "" {
		user = doc.Nodes[0].SSHUser
	}
	return ip, user
}

// remoteCI syncs the repo to the remote VM, runs lo provision remotely, and
// sets up expose + tunnel (bash: lo::remote_ci). The env-rewritten remote
// command lines are byte-identical to the bash \-continued double-quoted
// strings (line continuations collapse, the continuation indentation
// survives inside the string).
func (d *Driver) remoteCI(ctx context.Context, domain, clusterYAML string, out, errOut io.Writer) error {
	remote := getenv("LOK8S_REMOTE_USER") + "@" + getenv("LOK8S_REMOTE_IP")
	dest := getenv("LOK8S_REMOTE_SYNC_DEST")
	clusterName := yqRaw(loadYAML(clusterYAML), "metadata", "name")
	if clusterName == "null" {
		clusterName = ""
	}

	ui.Debugf(errOut, "CI mode: syncing repo to %s:%s", remote, dest)

	// dest expands CLIENT-side by design — it composes the remote command.
	// Its charset was validated at read time (readRemoteConfig), which is
	// what makes the single-quoted interpolation safe.
	if err := d.runOut(ctx, out, errOut, "ssh", remote, fmt.Sprintf("mkdir -p '%s'", dest)); err != nil {
		ui.Errorf(errOut, "failed to create %s on %s", dest, remote)
		return fmt.Errorf("remote mkdir failed: %w", err)
	}

	rsyncArgs := []string{"-az", "--delete", "--info=progress2"}
	for _, excl := range strings.Split(getenv("LOK8S_REMOTE_SYNC_EXCLUDE"), "\n") {
		if excl != "" {
			rsyncArgs = append(rsyncArgs, "--exclude="+excl)
		}
	}

	repoRoot, err := d.output(ctx, "git", "-C", d.deps.Paths.Base, "rev-parse", "--show-toplevel")
	if err != nil || repoRoot == "" {
		repoRoot = d.deps.Paths.Base
	}
	syncSrc := getenv("LOK8S_REMOTE_SYNC_PATH")
	if !strings.HasPrefix(syncSrc, "/") {
		syncSrc = repoRoot + "/" + syncSrc
	}
	if !strings.HasSuffix(syncSrc, "/") {
		syncSrc += "/"
	}

	if err := d.runOut(ctx, out, errOut, "rsync",
		append(append([]string{}, rsyncArgs...), syncSrc, remote+":"+dest+"/")...); err != nil {
		ui.Errorf(errOut, "rsync failed")
		return fmt.Errorf("rsync failed: %w", err)
	}

	if d.deps.Paths.Clusters != repoRoot+"/clusters" && dirExists(d.deps.Paths.Clusters) {
		// Best-effort: an external clusters dir rides along, failure tolerated.
		_ = d.runOut(ctx, out, errOut, "rsync", "-az", d.deps.Paths.Clusters+"/", remote+":"+dest+"/clusters/")
	}

	ui.Debugf(errOut, "repo synced to %s:%s", remote, dest)

	// Run lo provision on the remote VM (without --remote).
	ui.Debugf(errOut, "starting lo provision on %s", remote)
	provisionCmd := fmt.Sprintf("cd '%s' &&     export DOMAIN_NAME='%s' &&     export PATH_BASE='%s' &&     export PATH_LOK8S='%s/.lok8s' &&     export PATH_CLUSTERS='%s/clusters' &&     export PATH_BIN='%s/.bin' &&     export KUSTOMIZE_PLUGIN_HOME='%s/.kustomize' &&     export PATH=\"%s/.lok8s:%s/.bin:${PATH}\" &&     .lok8s/lo provision --domain '%s'",
		dest, domain, dest, dest, dest, dest, dest, dest, dest, domain)
	if err := d.runOut(ctx, out, errOut, "ssh", remote, provisionCmd); err != nil {
		ui.Errorf(errOut, "remote lo provision failed")
		return fmt.Errorf("remote lo provision failed: %w", err)
	}

	// Start Tilt if enabled.
	if getenv("LOK8S_REMOTE_TILT") == "true" {
		ui.Debugf(errOut, "starting Tilt on %s", remote)
		tiltCmd := fmt.Sprintf("cd '%s' &&     export DOMAIN_NAME='%s' &&     export PATH_BASE='%s' &&     export PATH_LOK8S='%s/.lok8s' &&     export PATH_CLUSTERS='%s/clusters' &&     nohup .lok8s/lo tilt up > /tmp/lok8s-tilt.log 2>&1 &",
			dest, domain, dest, dest, dest)
		if err := d.runOut(ctx, out, errOut, "ssh", remote, tiltCmd); err != nil {
			ui.Warnf(errOut, "remote Tilt start failed — cluster is provisioned but Tilt isn't running")
		}
		ui.Debugf(errOut, "Tilt started on %s (log: /tmp/lok8s-tilt.log)", remote)
	}

	// Expose if enabled. The config reads are best-effort here like the
	// bash (suppressed-errexit context): a failed read still proceeds into
	// expose with whatever env is set.
	if getenv("LOK8S_REMOTE_EXPOSE") == "true" {
		_ = readNetworkConfig(clusterYAML, errOut)
		readLBConfig(clusterYAML)
		// DOCKER_HOST only for the expose call (bash: the VAR=… command
		// prefix on a function — temporary in default bash mode).
		prev, had := os.LookupEnv("DOCKER_HOST")
		os.Setenv("DOCKER_HOST", "ssh://"+remote)
		_ = d.expose(ctx, clusterName, clusterYAML, out, errOut)
		if had {
			os.Setenv("DOCKER_HOST", prev)
		} else {
			os.Unsetenv("DOCKER_HOST")
		}
	}

	// Set up kubeconfig + SSH tunnel.
	kubeconfigPath := filepath.Join(d.deps.Paths.Base, ".kubeconfig", clusterName+".yaml")
	if !fileExists(kubeconfigPath) {
		_ = os.MkdirAll(filepath.Join(d.deps.Paths.Base, ".kubeconfig"), 0o755)
		_ = d.runQuiet(ctx, "scp", remote+":"+dest+"/.kubeconfig/"+clusterName+".yaml", kubeconfigPath)
	}

	if fileExists(kubeconfigPath) {
		_ = d.kubeconfigTunnel(ctx, kubeconfigPath, getenv("LOK8S_REMOTE_USER"), getenv("LOK8S_REMOTE_IP"), errOut)
	}

	accessIP := getenv("LOK8S_REMOTE_IP")
	fmt.Fprintln(out, ":: remote CI cluster ready")
	fmt.Fprintf(out, "   VM:         %s\n", accessIP)
	fmt.Fprintf(out, "   SSH:        ssh %s\n", remote)
	fmt.Fprintf(out, "   kubectl:    KUBECONFIG=%s kubectl get nodes\n", kubeconfigPath)
	if getenv("LOK8S_REMOTE_EXPOSE") == "true" {
		fmt.Fprintf(out, "   URL:        https://*.%s (via %s:443)\n", domain, accessIP)
	}
	if getenv("LOK8S_REMOTE_TILT") == "true" {
		fmt.Fprintf(out, "   Tilt log:   ssh %s tail -f /tmp/lok8s-tilt.log\n", remote)
	}
	fmt.Fprintf(out, "   Sync:       rsync -az %s %s:%s/\n", syncSrc, remote, dest)
	return nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
