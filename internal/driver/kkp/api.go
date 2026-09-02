package kkp

// api.go — the Go port of .lok8s/drivers/kkp/api: the KKP (Kubermatic
// Kubernetes Platform) v2 REST API client. The transport stays `curl`
// behind the execx.Runner seam — exact argv parity with the bash (and the
// bats suite stubs curl the same way) — with Bearer token auth, JSON
// handling, HTTP error checking, and 429 retry logic.
//
// All API calls go through (*Driver).api() which enforces HTTPS-only,
// redacts tokens from debug output, and handles common error patterns.
//
// Environment:
//   KKP_TOKEN   — Bearer token for KKP REST API authentication
//   KKP_API_URL — Base URL of the KKP API (https://kkp.example.com)
//   KKP_CA_CERT — optional custom CA bundle for the TLS handshake
//
// Tunables (the bash `: "${VAR:=default}"` set): KKP_MAX_RETRIES=3,
// KKP_RETRY_DELAY=2, KKP_WAIT_TIMEOUT=600, KKP_WAIT_INTERVAL=10 — read from
// the environment at call time.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func kkpMaxRetries() int   { return envInt("KKP_MAX_RETRIES", 3) }
func kkpRetryDelay() int   { return envInt("KKP_RETRY_DELAY", 2) }
func kkpWaitTimeout() int  { return envInt("KKP_WAIT_TIMEOUT", 600) }
func kkpWaitInterval() int { return envInt("KKP_WAIT_INTERVAL", 10) }

// validateURL ports kkp::validate_url → http::require_https.
func (d *Driver) validateURL(url string, stderr io.Writer) error {
	return requireHTTPS(url, "KKP API URL", stderr)
}

// requireHTTPS ports http::require_https.
func requireHTTPS(url, label string, stderr io.Writer) error {
	if !strings.HasPrefix(url, "https://") {
		ui.Errorf(stderr, "%s must use HTTPS: %s", label, url)
		ui.Errorf(stderr, "Plain HTTP is not allowed for security reasons")
		return fmt.Errorf("kkp: %s must use HTTPS: %s", label, url)
	}
	return nil
}

// api ports kkp::api: the generic KKP REST API caller with auth, JSON
// handling, and retry. Returns the response body. stderr is the diagnostic
// sink — the wait/status paths pass io.Discard, replicating the bash
// `2>/dev/null` suppression at those call sites.
func (d *Driver) api(ctx context.Context, method, path, body string, stderr io.Writer) (string, error) {
	token := os.Getenv("KKP_TOKEN")
	if token == "" {
		ui.Errorf(stderr, "KKP_TOKEN is not set")
		return "", fmt.Errorf("kkp: KKP_TOKEN is not set")
	}
	apiURL := os.Getenv("KKP_API_URL")
	if apiURL == "" {
		ui.Errorf(stderr, "KKP_API_URL is not set")
		return "", fmt.Errorf("kkp: KKP_API_URL is not set")
	}
	if err := d.validateURL(apiURL, stderr); err != nil {
		return "", err
	}

	url := apiURL + path
	curlArgs := []string{
		"--silent",
		"--show-error",
		"--fail-with-body",
		"--location",
		"--header", "Authorization: Bearer " + token,
		"--header", "Content-Type: application/json",
		"--header", "Accept: application/json",
		"--write-out", "\n%{http_code}",
		"--request", method,
	}
	if body != "" {
		curlArgs = append(curlArgs, "--data", body)
	}

	// Optional custom CA for the TLS handshake — needed when KKP serves a
	// self-signed / private-CA cert (local mkcert, CI) that isn't in the
	// system trust store. HTTPS is still enforced above; --cacert only
	// changes WHICH CA verifies the peer, it never disables verification.
	if ca := os.Getenv("KKP_CA_CERT"); ca != "" {
		if fileExists(ca) {
			curlArgs = append(curlArgs, "--cacert", ca)
		} else {
			ui.Errorf(stderr, "KKP_CA_CERT set but not a readable file: %s", ca)
			return "", fmt.Errorf("kkp: KKP_CA_CERT not a readable file: %s", ca)
		}
	}

	// Redact token from debug output.
	ui.Debugf(stderr, "KKP API: %s %s (token=<redacted>)", method, url)

	maxRetries := kkpMaxRetries()
	for attempt := 1; attempt <= maxRetries; attempt++ {
		// bash: output=$(curl … 2>&1) || true — stdout AND stderr captured
		// together, the exit status ignored; the trailing write-out line is
		// the verdict.
		var buf strings.Builder
		_ = d.deps.Runner.Run(ctx, execx.Cmd{
			Name: "curl", Args: append(append([]string{}, curlArgs...), url),
			Stdout: &buf, Stderr: &buf,
		})
		response, httpCode := splitHTTPCode(buf.String())

		if len(httpCode) == 3 && httpCode[0] == '2' && isDigits(httpCode) {
			ui.Debugf(stderr, "KKP API: %s %s -> %s", method, path, httpCode)
			return response, nil
		}

		// Rate limited — retry with backoff.
		if httpCode == "429" {
			delay := kkpRetryDelay() * attempt
			ui.Warnf(stderr, "KKP API rate limited (429), retrying in %ds (attempt %d/%d)", delay, attempt, maxRetries)
			d.sleepSeconds(delay)
			continue
		}

		// Client/server errors — no retry.
		ui.Errorf(stderr, "KKP API error: %s %s -> HTTP %s", method, path, httpCode)
		if response != "" {
			ui.Errorf(stderr, "Response: %s", response)
		}
		return "", fmt.Errorf("kkp: API error: %s %s -> HTTP %s", method, path, httpCode)
	}

	ui.Errorf(stderr, "KKP API: max retries (%d) exhausted for %s %s", maxRetries, method, path)
	return "", fmt.Errorf("kkp: max retries (%d) exhausted for %s %s", maxRetries, method, path)
}

// splitHTTPCode mirrors the bash split of curl's output: the LAST line is
// the `--write-out` status code (`http_code=$(… | tail -n1)`), everything
// before it the body (`response=$(… | sed '$d')`, whose $() strips trailing
// newlines).
func splitHTTPCode(out string) (response, code string) {
	trimmed := strings.TrimSuffix(out, "\n")
	if trimmed == "" {
		return "", ""
	}
	if i := strings.LastIndex(trimmed, "\n"); i >= 0 {
		return strings.TrimRight(trimmed[:i], "\n"), trimmed[i+1:]
	}
	return "", trimmed
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// ── Cluster operations ────────────────────────────────────

// createCluster ports kkp::create_cluster. Returns the new cluster ID.
func (d *Driver) createCluster(ctx context.Context, projectID, clusterJSON string) (string, error) {
	stderr := d.stderr()
	response, err := d.api(ctx, "POST", "/api/v2/projects/"+projectID+"/clusters", clusterJSON, stderr)
	if err != nil {
		return "", err
	}
	clusterID := jsonStringField(response, "id")
	if clusterID == "" {
		ui.Errorf(stderr, "KKP create_cluster: no cluster ID in response")
		ui.Errorf(stderr, "Response: %s", response)
		return "", fmt.Errorf("kkp: create_cluster: no cluster ID in response")
	}
	ui.Debugf(stderr, "KKP cluster created: %s", clusterID)
	return clusterID, nil
}

// deleteCluster ports kkp::delete_cluster.
func (d *Driver) deleteCluster(ctx context.Context, projectID, clusterID string) error {
	stderr := d.stderr()
	if _, err := d.api(ctx, "DELETE", "/api/v2/projects/"+projectID+"/clusters/"+clusterID, "", stderr); err != nil {
		return err
	}
	ui.Debugf(stderr, "KKP cluster deleted: %s", clusterID)
	return nil
}

// getCluster ports kkp::get_cluster (cluster JSON). stderr routes the
// suppressed call sites (`>/dev/null 2>&1`).
func (d *Driver) getCluster(ctx context.Context, projectID, clusterID string, stderr io.Writer) (string, error) {
	return d.api(ctx, "GET", "/api/v2/projects/"+projectID+"/clusters/"+clusterID, "", stderr)
}

// getKubeconfig ports kkp::get_kubeconfig: fetch and write to outputFile.
func (d *Driver) getKubeconfig(ctx context.Context, projectID, clusterID, outputFile string) error {
	stderr := d.stderr()
	kubeconfig, err := d.api(ctx, "GET", "/api/v2/projects/"+projectID+"/clusters/"+clusterID+"/kubeconfig", "", stderr)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outputFile), 0o755); err != nil {
		return err
	}
	// bash: echo "${kubeconfig}" > file — one trailing newline.
	if err := os.WriteFile(outputFile, []byte(kubeconfig+"\n"), 0o644); err != nil {
		return err
	}
	ui.Debugf(stderr, "KKP kubeconfig written to %s", outputFile)
	return nil
}

// ── Machine deployment operations ─────────────────────────

// createMachineDeployment ports kkp::create_machinedeployment.
func (d *Driver) createMachineDeployment(ctx context.Context, projectID, clusterID, mdJSON string) (string, error) {
	stderr := d.stderr()
	response, err := d.api(ctx, "POST", "/api/v2/projects/"+projectID+"/clusters/"+clusterID+"/machinedeployments", mdJSON, stderr)
	if err != nil {
		return "", err
	}
	mdID := jsonStringField(response, "id")
	if mdID == "" {
		ui.Errorf(stderr, "KKP create_machinedeployment: no ID in response")
		return "", fmt.Errorf("kkp: create_machinedeployment: no ID in response")
	}
	ui.Debugf(stderr, "KKP machine deployment created: %s", mdID)
	return mdID, nil
}

// listMachineDeployments ports kkp::list_machinedeployments.
func (d *Driver) listMachineDeployments(ctx context.Context, projectID, clusterID string, stderr io.Writer) (string, error) {
	return d.api(ctx, "GET", "/api/v2/projects/"+projectID+"/clusters/"+clusterID+"/machinedeployments", "", stderr)
}

// ── Health and status ─────────────────────────────────────

// health ports kkp::health (health JSON).
func (d *Driver) health(ctx context.Context, projectID, clusterID string, stderr io.Writer) (string, error) {
	return d.api(ctx, "GET", "/api/v2/projects/"+projectID+"/clusters/"+clusterID+"/health", "", stderr)
}

// componentUp is the shared per-component predicate: jq
// `.[$c] // "missing"` then `HealthStatusUp` or the legacy numeric `1`
// (`// "missing"` fires on null AND false, so both read as down).
func componentUp(health map[string]any, component string) bool {
	v, ok := health[component]
	if !ok || v == nil {
		return false
	}
	switch t := v.(type) {
	case string:
		return t == "HealthStatusUp"
	case float64:
		// jq -r prints integral numbers without a fraction: 1 -> "1".
		return t == 1 && t == math.Trunc(t)
	case bool:
		return false // falsy in jq's `//` — reads as "missing"
	default:
		return false
	}
}

// coreHealthy ports kkp::core_healthy: the core control-plane components
// report healthy. The v2 REST API does NOT expose the Cluster CR's
// .status.phase — the /health endpoint is the canonical readiness signal
// (verified against KKP 2.30). Provider-dependent components
// (machineController, OSM, …) are intentionally not gated on: they stay
// down for bringyourown.
func coreHealthy(healthJSON string) bool {
	var health map[string]any
	if json.Unmarshal([]byte(healthJSON), &health) != nil {
		// Invalid JSON made the bash jq call fail → status "" → unhealthy.
		return false
	}
	for _, component := range []string{"apiserver", "etcd", "controller", "scheduler"} {
		if !componentUp(health, component) {
			return false
		}
	}
	return true
}

// waitComponents ports kkp::wait_components: poll until the named health
// components are up or timeout. Stricter gate than coreHealthy —
// machine-deployment creation 503s ("Cluster components are not ready
// yet") until the provider components (machineController,
// operatingSystemManager) are up too.
func (d *Driver) waitComponents(ctx context.Context, projectID, clusterID string, timeout int, components ...string) error {
	stderr := d.stderr()
	start := d.now()

	ui.Debugf(stderr, "Waiting for KKP cluster %s components: %s (timeout=%ds)", clusterID, strings.Join(components, " "), timeout)

	for {
		elapsed := int(d.now().Sub(start).Seconds())
		if elapsed >= timeout {
			ui.Errorf(stderr, "Timed out waiting for KKP cluster %s components (%s) after %ds", clusterID, strings.Join(components, " "), timeout)
			return fmt.Errorf("kkp: timed out waiting for cluster %s components", clusterID)
		}

		// bash: health=$(kkp::health … 2>/dev/null) — diagnostics suppressed.
		if healthJSON, err := d.health(ctx, projectID, clusterID, io.Discard); err == nil {
			var health map[string]any
			_ = json.Unmarshal([]byte(healthJSON), &health)
			allUp := true
			lastComponent := ""
			for _, component := range components {
				if !componentUp(health, component) {
					allUp = false
					lastComponent = component
					break
				}
			}
			if allUp {
				return nil
			}
			ui.Debugf(stderr, "KKP cluster %s waiting on %s (elapsed=%ds)", clusterID, lastComponent, elapsed)
		}

		d.sleepSeconds(kkpWaitInterval())
	}
}

// waitReady ports kkp::wait_ready: poll until the core control plane is
// healthy or timeout, with exponential backoff (start KKP_WAIT_INTERVAL,
// double, cap 60s). timeout <= 0 defaults to KKP_WAIT_TIMEOUT.
func (d *Driver) waitReady(ctx context.Context, projectID, clusterID string, timeout int) error {
	stderr := d.stderr()
	if timeout <= 0 {
		timeout = kkpWaitTimeout()
	}
	start := d.now()
	interval := kkpWaitInterval()
	lastHealth := ""

	ui.Debugf(stderr, "Waiting for KKP cluster %s control plane to become healthy (timeout=%ds)", clusterID, timeout)

	for {
		elapsed := int(d.now().Sub(start).Seconds())
		if elapsed >= timeout {
			health := lastHealth
			if health == "" {
				health = "unknown" // bash: ${health:-unknown}
			}
			ui.Errorf(stderr, "Timed out waiting for KKP cluster %s to become healthy after %ds (last health: %s)", clusterID, timeout, health)
			return fmt.Errorf("kkp: timed out waiting for cluster %s to become healthy", clusterID)
		}

		if healthJSON, err := d.health(ctx, projectID, clusterID, io.Discard); err == nil {
			lastHealth = healthJSON
			if coreHealthy(healthJSON) {
				ui.Debugf(stderr, "KKP cluster %s control plane is healthy", clusterID)
				return nil
			}
			ui.Debugf(stderr, "KKP cluster %s health: %s (elapsed=%ds)", clusterID, healthJSON, elapsed)
		} else {
			ui.Warnf(stderr, "KKP cluster health check failed, retrying...")
		}

		d.sleepSeconds(interval)

		// Exponential backoff (cap at 60s).
		interval *= 2
		if interval > 60 {
			interval = 60
		}
	}
}

// ── Credential validation ─────────────────────────────────

// validateCredentials ports kkp::validate_credentials: the env-var and
// spec checks for KKP operations, all failures reported before the count
// summary.
func (d *Driver) validateCredentials(clusterYAML string) error {
	stderr := d.stderr()
	spec := loadSpec(clusterYAML)
	errors := 0

	// KKP API token is always required.
	if os.Getenv("KKP_TOKEN") == "" {
		ui.Errorf(stderr, "KKP_TOKEN env var is required for KKP API authentication")
		errors++
	}

	// KKP API URL is always required.
	if os.Getenv("KKP_API_URL") == "" {
		if spec.or("", "spec", "kkp", "apiUrl") == "" {
			ui.Errorf(stderr, "KKP_API_URL env var or spec.kkp.apiUrl is required")
			errors++
		}
	}

	// Validate HTTPS on the API URL if set.
	apiURL := os.Getenv("KKP_API_URL")
	if apiURL == "" {
		apiURL = spec.or("", "spec", "kkp", "apiUrl")
	}
	if apiURL != "" {
		if err := d.validateURL(apiURL, stderr); err != nil {
			errors++
		}
	}

	// Optional custom CA bundle: spec.kkp.caCert seeds KKP_CA_CERT (env
	// wins). Lets the API client verify a self-signed / private-CA KKP
	// endpoint (local mkcert, CI) without trusting it system-wide. Relative
	// paths resolve against the cluster spec's directory.
	if os.Getenv("KKP_CA_CERT") == "" {
		if specCA := spec.or("", "spec", "kkp", "caCert"); specCA != "" {
			if !strings.HasPrefix(specCA, "/") {
				specCA = filepath.Join(filepath.Dir(clusterYAML), specCA)
			}
			if fileExists(specCA) {
				os.Setenv("KKP_CA_CERT", specCA)
				ui.Debugf(stderr, "KKP CA cert from spec.kkp.caCert: %s", specCA)
			} else {
				ui.Errorf(stderr, "spec.kkp.caCert points to a missing file: %s", specCA)
				errors++
			}
		}
	}

	// Provider-specific credential checks (unless using a KKP preset).
	if preset := spec.or("", "spec", "kkp", "preset"); preset == "" {
		provider := spec.providerName("")
		switch {
		case provider == "byo" || provider == "bringyourown":
			ui.Debugf(stderr, "bringyourown provider — no cloud credentials required")
		case provider != "":
			if err := requireCredentials(provider, stderr); err != nil {
				errors++
			}
		default:
			ui.Debugf(stderr, "No provider specified in spec — skipping provider credential check")
		}
	} else {
		ui.Debugf(stderr, "Using KKP preset '%s' — skipping provider credential check", preset)
	}

	if errors > 0 {
		ui.Errorf(stderr, "%d credential validation error(s)", errors)
		return fmt.Errorf("kkp: %d credential validation error(s)", errors)
	}

	ui.Debugf(stderr, "KKP credentials validated")
	return nil
}

// requireCredentials ports credentials::require (utils/credentials.sh).
func requireCredentials(provider string, stderr io.Writer) error {
	var missing []string
	switch provider {
	case "hetzner":
		if os.Getenv("HCLOUD_TOKEN") == "" {
			missing = append(missing, "HCLOUD_TOKEN")
		}
	case "aws":
		if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
			missing = append(missing, "AWS_ACCESS_KEY_ID")
		}
		if os.Getenv("AWS_SECRET_ACCESS_KEY") == "" {
			missing = append(missing, "AWS_SECRET_ACCESS_KEY")
		}
	default:
		ui.Errorf(stderr, "unknown provider '%s' for credential check", provider)
		return fmt.Errorf("kkp: unknown provider %q for credential check", provider)
	}
	if len(missing) > 0 {
		for _, v := range missing {
			ui.Errorf(stderr, "required environment variable %s is not set", v)
		}
		return fmt.Errorf("kkp: missing credentials: %s", strings.Join(missing, ", "))
	}
	return nil
}

// jsonStringField mirrors `jq -r '.<field> // empty'` on a JSON object:
// the string value, or "" for missing/null/non-string/invalid JSON.
func jsonStringField(jsonText, field string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(jsonText), &obj) != nil {
		return ""
	}
	if s, ok := obj[field].(string); ok {
		return s
	}
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
