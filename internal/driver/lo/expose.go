package lo

// expose.go — nginx reverse proxy for remote cluster access
// (.lok8s/drivers/lo/utils/expose.sh).
//
// nginx.conf serves HTTPS only, so the proxy needs a TLS certificate and
// key. Without both files, expose does not start the proxy: it warns and
// returns with no error. Each docker step that sets up the proxy (the run,
// each copy and the reload) must succeed. A failed step removes the proxy
// container and stops expose with an error that names the step and the
// container, and expose does not print "cluster exposed". The bash twin
// checks each step itself: its callers run it under `|| return 1`, which
// turns errexit off in the function.

import (
	"context"
	"fmt"
	"github.com/kernpilot/lok8s/internal/config"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// expose starts the nginx proxy container in front of the cluster (bash:
// lo::expose). It returns true when the proxy runs. Without the TLS
// certificate or key, it does not start the proxy and returns false with no
// error (bash: LO_EXPOSE_STARTED).
func (d *Driver) expose(ctx context.Context, clusterName, clusterYAML string, out, errOut io.Writer) (bool, error) {
	domain := yqsem.Raw(yqsem.Lookup(yqsem.LoadNode(clusterYAML), "spec", "cluster", "domain"))

	proxyName := clusterName + "-proxy"
	network := getenv("KIND_EXPERIMENTAL_DOCKER_NETWORK")

	backendIP := getenv("LOK8S_LB_POOL")
	if backendIP != "" {
		if i := strings.Index(backendIP, "-"); i >= 0 {
			backendIP = backendIP[:i]
		}
	} else {
		backendIP = getenv("LOK8S_NETWORK_BASE_IP")
		if backendIP == "" {
			backendIP = "127.0.0.1"
		}
	}

	// The TLS certificate and key of the proxy. The driver does not make
	// them: put a tls.crt and a tls.key here (for example from a cert:
	// Secret). nginx.conf serves HTTPS only, so without both files the
	// proxy does not start.
	certPath := filepath.Join(d.deps.Paths.Base, ".secrets", "tls", "tls.crt")
	keyPath := filepath.Join(d.deps.Paths.Base, ".secrets", "tls", "tls.key")

	nginxTemplate, _, err := assets.Resolve(d.deps.Paths, "drivers/lo/cluster/expose/nginx.conf")
	if err != nil {
		return false, err
	}
	if !fsutil.FileExists(nginxTemplate) {
		ui.ErrorTo(errOut, "expose: nginx template not found at %s", nginxTemplate)
		return false, ui.Handled(fmt.Errorf("nginx template not found at %s", nginxTemplate))
	}

	// Remove the proxy of an earlier run first, also when this run starts
	// no proxy, so that no stale proxy stays.
	_ = d.runQuiet(ctx, "docker", "rm", "-f", proxyName)

	if !fsutil.FileExists(certPath) || !fsutil.FileExists(keyPath) {
		ui.WarnTo(errOut, "expose: no TLS certificate and key at %s and %s, so the proxy is not started. Add both files, or set spec.remote.expose: false.", certPath, keyPath)
		return false, nil
	}

	// Render the nginx config — envsubst restricted to EXACTLY the two-var
	// whitelist, like the bash `envsubst '${LOK8S_EXPOSE_DOMAIN}
	// ${LOK8S_EXPOSE_BACKEND_IP}'`: any other $… in the template (nginx's
	// own $host/$http_upgrade variables) passes through untouched.
	raw, err := os.ReadFile(nginxTemplate)
	if err != nil {
		return false, err
	}
	os.Setenv("LOK8S_EXPOSE_DOMAIN", domain)
	os.Setenv("LOK8S_EXPOSE_BACKEND_IP", backendIP)
	rendered := build.Envsubst(raw, []string{"LOK8S_EXPOSE_DOMAIN", "LOK8S_EXPOSE_BACKEND_IP"})
	os.Unsetenv("LOK8S_EXPOSE_DOMAIN")
	os.Unsetenv("LOK8S_EXPOSE_BACKEND_IP")

	tmpConf, err := os.CreateTemp("", "lok8s-nginx.*.conf")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(tmpConf.Name()) }()
	if _, err := tmpConf.Write(rendered); err != nil {
		_ = tmpConf.Close()
		return false, err
	}
	if err := tmpConf.Close(); err != nil {
		return false, err
	}

	// Start container with default nginx config, then overwrite it.
	// docker cp streams file content over the Docker connection (works with
	// DOCKER_HOST=ssh:// where -v mounts can't reach local paths). The image
	// is ExposeImage (defaults.go), pinned by tag and digest.
	if err := d.runOut(ctx, out, errOut, "docker", "run", "-d", "--restart=always",
		"--name", proxyName,
		"--network", network,
		"-p", "80:80", "-p", "443:443",
		ExposeImage); err != nil {
		return false, d.exposeStepFailed(ctx, errOut, proxyName, "docker run of the proxy container "+proxyName, err)
	}

	// Copy the rendered config and the TLS files into the running
	// container. The ssl_certificate and ssl_certificate_key paths of
	// nginx.conf are the targets of the two TLS copies.
	if err := d.runOut(ctx, out, errOut, "docker", "cp", tmpConf.Name(), proxyName+":/etc/nginx/nginx.conf"); err != nil {
		return false, d.exposeStepFailed(ctx, errOut, proxyName, "docker cp of the nginx config to "+proxyName+":/etc/nginx/nginx.conf", err)
	}
	if err := d.runOut(ctx, out, errOut, "docker", "cp", certPath, proxyName+":/tls.crt"); err != nil {
		return false, d.exposeStepFailed(ctx, errOut, proxyName, "docker cp of the TLS certificate to "+proxyName+":/tls.crt", err)
	}
	if err := d.runOut(ctx, out, errOut, "docker", "cp", keyPath, proxyName+":/tls.key"); err != nil {
		return false, d.exposeStepFailed(ctx, errOut, proxyName, "docker cp of the TLS key to "+proxyName+":/tls.key", err)
	}

	// Reload nginx with the new config.
	if err := d.runOut(ctx, out, errOut, "docker", "exec", proxyName, "nginx", "-s", "reload"); err != nil {
		return false, d.exposeStepFailed(ctx, errOut, proxyName, "docker exec "+proxyName+" nginx -s reload", err)
	}

	accessIP := config.EnvOr("LOK8S_REMOTE_IP", "localhost")
	ui.DebugTo(errOut, "expose: nginx proxy %s running on %s:443 → %s", proxyName, accessIP, backendIP)
	fmt.Fprintf(out, ":: cluster exposed at https://*.%s (via %s:443)\n", domain, accessIP)
	return true, nil
}

// exposeStepFailed reports a failed docker step of expose: the step, its
// exit status and the next action (bash: lo::expose_step_failed). First it
// removes the proxy container (the error of that removal does not count):
// a container that a failed step left would restart always with a config
// that is not complete.
func (d *Driver) exposeStepFailed(ctx context.Context, errOut io.Writer, proxyName, step string, err error) error {
	_ = d.runQuiet(ctx, "docker", "rm", "-f", proxyName)
	ui.ErrorTo(errOut, "expose: %s failed (exit status %d), so the proxy does not run. Fix the cause in the docker message above, then run the command again.",
		step, execx.ExitCode(err))
	return ui.Handled(fmt.Errorf("expose: %s: %w", step, err))
}
