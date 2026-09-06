package lo

// expose.go — nginx reverse proxy for remote cluster access
// (.lok8s/drivers/lo/utils/expose.sh).
//
// ERREXIT-SUPPRESSION PARITY NOTE: like lo::coredns, the bash lo::expose
// ran under the caller's `|| return 1`, so its docker steps were
// effectively best-effort — a failed `docker run`/`docker cp` did not abort
// the function, whose status came from the final echo (0). Only the missing
// nginx template returned 1. The port mirrors that exactly.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/ui"
)

// expose starts the nginx proxy container in front of the cluster (bash:
// lo::expose).
func (d *Driver) expose(ctx context.Context, clusterName, clusterYAML string, out, errOut io.Writer) error {
	domain := yqRaw(loadYAML(clusterYAML), "spec", "cluster", "domain")

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

	// Optional TLS for the proxy. The driver no longer mints this cert;
	// drop a tls.crt/tls.key here (e.g. extracted from a cert: Secret) to
	// serve HTTPS. Absent → the proxy runs plain HTTP (warning below).
	certPath := filepath.Join(d.deps.Paths.Base, ".secrets", "tls", "tls.crt")
	keyPath := filepath.Join(d.deps.Paths.Base, ".secrets", "tls", "tls.key")

	nginxTemplate, _, err := assets.Resolve(d.deps.Paths, "drivers/lo/cluster/expose/nginx.conf")
	if err != nil {
		return err
	}
	if !fileExists(nginxTemplate) {
		ui.Errorf(errOut, "expose: nginx template not found at %s", nginxTemplate)
		return fmt.Errorf("nginx template not found at %s", nginxTemplate)
	}

	// Render the nginx config — envsubst restricted to EXACTLY the two-var
	// whitelist, like the bash `envsubst '${LOK8S_EXPOSE_DOMAIN}
	// ${LOK8S_EXPOSE_BACKEND_IP}'`: any other $… in the template (nginx's
	// own $host/$http_upgrade variables) passes through untouched.
	raw, err := os.ReadFile(nginxTemplate)
	if err != nil {
		return err
	}
	os.Setenv("LOK8S_EXPOSE_DOMAIN", domain)
	os.Setenv("LOK8S_EXPOSE_BACKEND_IP", backendIP)
	rendered := build.Envsubst(raw, []string{"LOK8S_EXPOSE_DOMAIN", "LOK8S_EXPOSE_BACKEND_IP"})
	os.Unsetenv("LOK8S_EXPOSE_DOMAIN")
	os.Unsetenv("LOK8S_EXPOSE_BACKEND_IP")

	tmpConf, err := os.CreateTemp("", "lok8s-nginx.*.conf")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmpConf.Name()) }()
	if _, err := tmpConf.Write(rendered); err != nil {
		_ = tmpConf.Close()
		return err
	}
	if err := tmpConf.Close(); err != nil {
		return err
	}

	_ = d.runQuiet(ctx, "docker", "rm", "-f", proxyName)

	// Start container with default nginx config, then overwrite it.
	// docker cp streams file content over the Docker connection (works with
	// DOCKER_HOST=ssh:// where -v mounts can't reach local paths).
	_ = d.runOut(ctx, out, errOut, "docker", "run", "-d", "--restart=always",
		"--name", proxyName,
		"--network", network,
		"-p", "80:80", "-p", "443:443",
		"nginx:alpine")

	// Copy rendered config + optional TLS certs into the running container.
	_ = d.runOut(ctx, out, errOut, "docker", "cp", tmpConf.Name(), proxyName+":/etc/nginx/nginx.conf")

	if fileExists(certPath) && fileExists(keyPath) {
		// KNOWN DEFECT, PRESERVED ON PURPOSE: the shipped nginx.conf
		// references `ssl_certificate /tls.cert` (with an E), but the copy
		// below lands the file at /tls.CRT — so nginx's TLS server block
		// cannot find its cert and the reload fails on HTTPS. The bash has
		// shipped this mismatch since the template landed; fixing either
		// side is a user-visible behavior change (the proxy suddenly serves
		// TLS) that belongs to its own change, not this port. Do NOT
		// silently align the paths.
		_ = d.runOut(ctx, out, errOut, "docker", "cp", certPath, proxyName+":/tls.crt")
		_ = d.runOut(ctx, out, errOut, "docker", "cp", keyPath, proxyName+":/tls.key")
	} else {
		ui.Warnf(errOut, "expose: TLS certs not found at %s — proxy will run without TLS", certPath)
	}

	// Reload nginx with the new config.
	_ = d.runOut(ctx, out, errOut, "docker", "exec", proxyName, "nginx", "-s", "reload")

	accessIP := envOr("LOK8S_REMOTE_IP", "localhost")
	ui.Debugf(errOut, "expose: nginx proxy %s running on %s:443 → %s", proxyName, accessIP, backendIP)
	fmt.Fprintf(out, ":: cluster exposed at https://*.%s (via %s:443)\n", domain, accessIP)
	return nil
}
