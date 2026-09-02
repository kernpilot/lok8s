package kubehz

// hosted.go — libs/kubehz/hosted: creating/destroying clusters via the
// kubehz api for hosting: hosted (the flows the kubeone/capi drivers call
// through their Hooks).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// buildClusterPayload ports kubehz::build_cluster_payload. `kind` is the
// WIRE field in the spec's ORIGINAL case ("KubeOne") — a cross-repo
// contract, never lowercased. controlPlaneReplicas rides as a JSON number
// (`--argjson`): a non-numeric scalar made jq fail and the payload EMPTY in
// bash; mirrored here as nil.
func buildClusterPayload(clusterYAML string) []byte {
	doc := loadSpec(clusterYAML)
	replicas := doc.or("1", "spec", "controlPlane", "replicas")
	if replicas == "" {
		// Unreadable file: every yq read is "" → --argjson "" → jq error.
		return nil
	}
	var replicasJSON json.RawMessage
	if json.Valid([]byte(replicas)) {
		replicasJSON = json.RawMessage(replicas)
	} else {
		return nil
	}
	return compactJSON(
		jsonPair{"domain", doc.raw("spec", "cluster", "domain")},
		jsonPair{"kind", doc.raw("kind")},
		jsonPair{"provider", doc.or("hetzner", "spec", "provider")},
		jsonPair{"region", doc.orChain("fsn1", []string{"spec", "hcloud", "region"}, []string{"spec", "aws", "region"})},
		jsonPair{"kubernetesVersion", doc.raw("spec", "kubernetes", "version")},
		jsonPair{"controlPlaneReplicas", replicasJSON},
		jsonPair{"hosting", doc.or("self", "spec", "kubehz", "hosting")},
		jsonPair{"access", doc.or("none", "spec", "kubehz", "access")},
	)
}

// waitForCluster ports kubehz::wait_for_cluster: poll GET /api/clusters/{id}
// at 10-second intervals up to timeout seconds.
func (c *Context) waitForCluster(ctx context.Context, apiURL, clusterID string, timeout int) error {
	c.debugf("Waiting for hosted cluster %s to become ready (timeout: %ds)", clusterID, timeout)
	auth := withBearer(c.getenv("KUBEHZ_TOKEN"))
	for elapsed := 0; elapsed < timeout; elapsed += 10 {
		statusVal := "Unknown"
		if body, err := c.fetch(ctx, "GET", apiURL+"/api/clusters/"+clusterID, auth, nil); err == nil {
			// Enveloped UI route: .data.status, with a bare-body fallback.
			if v, ok := parseJSON(body); ok {
				statusVal = jstr(jalt("Unknown", jget(v, "data", "status"), jget(v, "status")))
			}
		}
		switch statusVal {
		case "Running", "ready", "Ready":
			c.debugf("Hosted cluster %s is ready", clusterID)
			return nil
		case "Failed", "Error":
			c.errorf("Hosted cluster %s failed: status=%s", clusterID, statusVal)
			return ErrHandled
		}
		c.debugf("Cluster %s status: %s (%ds / %ds)", clusterID, statusVal, elapsed, timeout)
		c.sleep(10 * time.Second)
	}
	c.errorf("Timed out waiting for hosted cluster %s after %ds", clusterID, timeout)
	return ErrHandled
}

var digitsRe = regexp.MustCompile(`^[0-9]+$`)

// renderCapacityRejection ports kubehz::render_capacity_rejection: the
// friendly message for the api's 503 AT_CAPACITY envelope. The "live
// availability" pointer uses the module-global api URL (cfg.APIURL).
func (c *Context) renderCapacityRejection(cfg *Config, body []byte) {
	v, _ := parseJSON(body)
	tier := jstr(jalt("this plan", jget(v, "data", "detail", "tier"), jget(v, "detail", "tier")))
	used := jstr(jalt("", jget(v, "data", "detail", "used"), jget(v, "detail", "used")))
	limit := jstr(jalt("", jget(v, "data", "detail", "limit"), jget(v, "detail", "limit")))
	retry := jstr(jalt("", jget(v, "data", "detail", "retryAfter"), jget(v, "detail", "retryAfter")))

	usage := ""
	if used != "" && limit != "" {
		usage = " (currently " + used + "/" + limit + ")"
	}
	// Humanize retryAfter seconds into "~N min" / "~Ns".
	retryHint := ""
	if digitsRe.MatchString(retry) {
		n, _ := strconv.Atoi(retry)
		if n >= 60 {
			retryHint = "~" + strconv.Itoa((n+30)/60) + " min"
		} else {
			retryHint = "~" + strconv.Itoa(n) + "s"
		}
	}

	c.errorf("kubehz: the platform is at capacity for the '%s' plan right now%s.", tier, usage)
	c.echoErr("  The hosted control-plane pool for this plan is full. You can:")
	c.echoErr("    • retry later — capacity frees as clusters are torn down")
	if retryHint != "" {
		c.echoErr("    • suggested wait: %s", retryHint)
	}
	c.echoErr("    • check live per-plan availability and retry when a slot opens:")
	c.echoErr("        %s/api/capacity", cfg.APIURL)
	c.echoErr("    • run a self-hosted cluster meanwhile (spec.kubehz.hosting: self)")
	c.echoErr("    • still stuck? contact kubehz support with the tier + time above")
}

// ProvisionHosted ports kubehz::provision_hosted: create the cluster via
// POST /api/clusters, wait for it, download the kubeconfig to
// .kubeconfig/<domain>.yaml (0600) and mirror it to
// .kubeconfig/<metadata.name>.yaml for the dispatch tail.
func (c *Context) ProvisionHosted(ctx context.Context, cfg *Config, domain, clusterYAML string) error {
	apiURL := cfg.APIURL
	payload := buildClusterPayload(clusterYAML)
	if payload == nil {
		payload = []byte{}
	}
	c.debugf("Creating hosted cluster for %s", domain)

	// Both body and status: a 503 AT_CAPACITY body is the whole point.
	res, err := c.fetchStatus(ctx, "POST", apiURL+"/api/clusters", withBearer(c.getenv("KUBEHZ_TOKEN")), payload)
	if err != nil {
		c.errorf("Failed to create hosted cluster via kubehz API (network error)")
		return ErrHandled
	}
	if res.Status == 503 && apiCode(res.Body) == "AT_CAPACITY" {
		c.renderCapacityRejection(cfg, res.Body)
		return ErrHandled
	}
	if !is2xx(res.Status) {
		msg := apiMessage(res.Body)
		help := apiHelp(res.Body)
		c.errorf("Failed to create hosted cluster via kubehz API (HTTP %d)%s", res.Status, optSuffix(": ", msg))
		if help != "" {
			c.echoErr("  %s", help)
		}
		return ErrHandled
	}

	v, _ := parseJSON(res.Body)
	clusterID := jstr(jalt(nil, jget(v, "data", "id"), jget(v, "id")))
	if clusterID == "" || clusterID == "null" {
		c.errorf("kubehz API did not return a cluster ID")
		return ErrHandled
	}
	c.debugf("Hosted cluster created: %s", clusterID)

	// Gate explicitly: a Failed hosted cluster must not "succeed" into the
	// kubeconfig download.
	if err := c.waitForCluster(ctx, apiURL, clusterID, 600); err != nil {
		return err
	}

	kubeconfigPath := filepath.Join(c.Paths.Base, ".kubeconfig", domain+".yaml")
	if err := os.MkdirAll(filepath.Dir(kubeconfigPath), 0o755); err != nil {
		return err
	}
	kc, err := c.fetch(ctx, "GET", apiURL+"/api/clusters/"+clusterID+"/kubeconfig", withBearer(c.getenv("KUBEHZ_TOKEN")), nil)
	if err != nil {
		// bash: the `>` redirection creates the file before curl fails.
		_ = os.WriteFile(kubeconfigPath, nil, 0o644)
		c.errorf("Failed to download kubeconfig for hosted cluster %s", clusterID)
		return ErrHandled
	}
	if err := os.WriteFile(kubeconfigPath, kc, 0o644); err != nil {
		c.errorf("Failed to download kubeconfig for hosted cluster %s", clusterID)
		return ErrHandled
	}
	c.debugf("Hosted cluster %s ready, kubeconfig at %s", domain, kubeconfigPath)

	_ = os.Chmod(kubeconfigPath, 0o600)
	clusterName := loadSpec(clusterYAML).raw("metadata", "name")
	if clusterName != "" && clusterName != "null" && clusterName != domain {
		mirror := filepath.Join(c.Paths.Base, ".kubeconfig", clusterName+".yaml")
		if err := os.WriteFile(mirror, kc, 0o600); err == nil {
			_ = os.Chmod(mirror, 0o600)
		} else {
			c.echoErr("[kubehz] warning: could not mirror the kubeconfig to .kubeconfig/%s.yaml — the bootstrap step will report it missing", clusterName)
		}
	}
	return nil
}

// DestroyHosted ports kubehz::destroy_hosted: resolve the cluster id from
// the tenant registry, DELETE it, and remove BOTH local kubeconfigs (the
// domain-keyed download and the metadata.name mirror — libs/build PREFERS
// the mirror, so a stale one would feed builds the dead cluster's creds).
func (c *Context) DestroyHosted(ctx context.Context, cfg *Config, domain, clusterYAML string) error {
	apiURL := cfg.APIURL
	if err := c.requireHTTPS(apiURL, "spec.kubehz.apiUrl"); err != nil {
		return err
	}

	clusterID, err := c.ResolveClusterID(ctx, domain, apiURL)
	if err != nil && err != errNotRegistered {
		c.errorf("kubehz: cannot read the cluster registry at %s — nothing was deleted for %s and the local kubeconfigs are kept. Set KUBEHZ_TOKEN to a clusters:write token of the owning tenant, then retry.", apiURL, domain)
		return ErrHandled
	}
	if err == errNotRegistered {
		c.echo("kubehz: no hosted cluster is registered for %s — nothing to destroy on the platform.", domain)
	} else {
		res, err := c.fetchStatus(ctx, "DELETE", apiURL+"/api/clusters/"+clusterID, withBearer(c.getenv("KUBEHZ_TOKEN")), nil)
		if err != nil {
			c.errorf("kubehz: DELETE /api/clusters/%s failed (network) — the hosted cluster can still exist and bill. Retry when %s is reachable.", clusterID, apiURL)
			return ErrHandled
		}
		if !is2xx(res.Status) {
			msg := apiMessage(res.Body)
			c.errorf("kubehz: delete refused for %s (%s) — HTTP %d%s. The hosted cluster can still exist and bill; check the dashboard.", domain, clusterID, res.Status, optSuffix(": ", msg))
			return ErrHandled
		}
		c.debugf("Hosted cluster %s (%s) destroyed", domain, clusterID)
	}

	_ = os.Remove(filepath.Join(c.Paths.Base, ".kubeconfig", domain+".yaml"))
	mirrorName := loadSpec(clusterYAML).or("", "metadata", "name")
	if mirrorName != "" && mirrorName != "null" && mirrorName != domain {
		_ = os.Remove(filepath.Join(c.Paths.Base, ".kubeconfig", mirrorName+".yaml"))
	}
	return nil
}
