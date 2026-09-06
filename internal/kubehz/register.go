package kubehz

// register.go — the cluster-registration half of libs/kubehz/main:
// kubehz::ensure_claim_key, kubehz::direct_claim, kubehz::register_cluster,
// kubehz::deregister_cluster and kubehz::resolve_cluster_id.

import (
	"context"
	"errors"
	"net/url"
)

// hcloudAPIBase is `${HCLOUD_API_BASE:-https://api.hetzner.cloud}`.
func (c *Context) hcloudAPIBase() string {
	if v := c.getenv("HCLOUD_API_BASE"); v != "" {
		return v
	}
	return "https://api.hetzner.cloud"
}

// ensureClaimKey ports kubehz::ensure_claim_key — the PRIMARY registration
// for hcloud-backed clusters: the api mints the claim key SERVER-SIDE
// ({domain, claimKey: true}) and we upload the returned PUBLIC key into the
// owner's Hetzner Cloud account under kubehz-claim-<domain>. Re-runs ROTATE
// server-side, so the hcloud key is replaced by name on every run.
// Fail-soft: any failure returns an error and the caller falls back.
func (c *Context) ensureClaimKey(ctx context.Context, domain, apiURL string) error {
	hcloudAPI := c.hcloudAPIBase()
	// HCLOUD_TOKEN travels on this base below — never over plain HTTP.
	if err := c.requireHTTPS(hcloudAPI, "HCLOUD_API_BASE"); err != nil {
		return err
	}

	body, err := c.fetch(ctx, "POST", apiURL+"/api/clusters/register", bearer{},
		compactJSON(jsonPair{"domain", domain}, jsonPair{"claimKey", true}))
	if err != nil {
		return err
	}
	v, _ := parseJSON(body)
	clusterID := jstrOr(v, "", "id")
	publicKey := jstrOr(v, "", "claimKey", "publicKey")
	fingerprint := jstrOr(v, "", "claimKey", "fingerprint")
	keyName := jstrOr(v, "", "claimKey", "name")
	if clusterID == "" || publicKey == "" || fingerprint == "" || keyName == "" {
		return errors.New("kubehz: incomplete claim-key response")
	}

	hcloudAuth := withBearer(c.getenv("HCLOUD_TOKEN"))
	// Replace-by-name: a stale kubehz-claim key must not linger.
	existingID := ""
	if raw, err := c.fetch(ctx, "GET", hcloudAPI+"/v1/ssh_keys?name="+url.QueryEscape(keyName), hcloudAuth, nil); err == nil {
		if lv, ok := parseJSON(raw); ok {
			if keys, ok := jget(lv, "ssh_keys").([]any); ok && len(keys) > 0 {
				existingID = jstrOr(keys[0], "", "id")
			}
		}
	}
	if existingID != "" {
		_, _ = c.fetch(ctx, "DELETE", hcloudAPI+"/v1/ssh_keys/"+existingID, hcloudAuth, nil)
	}
	if _, err := c.fetch(ctx, "POST", hcloudAPI+"/v1/ssh_keys", hcloudAuth,
		compactJSON(jsonPair{"name", keyName}, jsonPair{"public_key", publicKey})); err != nil {
		return err
	}

	c.debugf("Cluster registered with kubehz claim key (pending claim): %s", clusterID)
	c.echo("kubehz: cluster '%s' registered (pending). Claim key '%s' uploaded to your Hetzner Cloud account.", domain, keyName)
	c.echo("kubehz: claim it with the fingerprint ALONE — dashboard /claim (SSH fingerprint tab), or:")
	c.echo("  curl -X POST %s/api/claims/verify -H 'Authorization: Bearer <khzt_ token (clusters:write)>' \\", apiURL)
	c.echo("       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"%s\"}'", fingerprint)
	c.echo("  fingerprint: %s   (also visible in Hetzner Console -> Security -> SSH keys)", fingerprint)
	return nil
}

// directClaim ports kubehz::direct_claim — register a cluster ATTRIBUTED
// DIRECTLY to the caller's tenant with a KUBEHZ_TOKEN (clusters:write).
// When spec.kubehz.connectHcloudToken is true AND HCLOUD_TOKEN is present,
// it then hands the platform the hcloud token (sent ONLY on the HTTPS api
// URL, never printed). Fail-soft: an error means "fall back".
func (c *Context) directClaim(ctx context.Context, cfg *Config, domain, clusterYAML, apiURL string) error {
	fingerprint, err := c.SSHFingerprint(ctx, clusterYAML)
	if err != nil {
		c.warnf("Could not extract SSH fingerprint for %s", domain)
		return ErrHandled
	}

	auth := withBearer(c.getenv("KUBEHZ_TOKEN"))
	body, err := c.fetch(ctx, "POST", apiURL+"/api/clusters/register", auth,
		compactJSON(jsonPair{"domain", domain}, jsonPair{"fingerprint", fingerprint}))
	if err != nil {
		return err
	}
	v, _ := parseJSON(body)
	clusterID := jstrOr(v, "", "id")
	claimed := jstrOr(v, "false", "claimed")
	if clusterID == "" {
		return errors.New("kubehz: register returned no id")
	}
	if claimed != "true" {
		c.warnf("kubehz register did not attribute %s to your tenant (is KUBEHZ_TOKEN a clusters:write token?)", domain)
		return ErrHandled
	}
	c.echo("kubehz: cluster '%s' registered and claimed to your account (%s).", domain, clusterID)

	// Optionally connect the hcloud token for dashboard-driven provisioning.
	connect := cfg.ConnectToken
	if connect == "" {
		connect = "false"
	}
	if connect == "true" {
		hcloudToken := c.getenv("HCLOUD_TOKEN")
		if hcloudToken == "" {
			c.warnf("spec.kubehz.connectHcloudToken is true but HCLOUD_TOKEN is unset — skipping token connect.")
			return nil
		}
		cred, err := c.fetch(ctx, "POST", apiURL+"/api/credentials", auth,
			compactJSON(jsonPair{"type", "hcloud_token"}, jsonPair{"value", hcloudToken},
				jsonPair{"validate", true}, jsonPair{"clusterId", clusterID}))
		if err != nil {
			c.warnf("kubehz: could not connect the hcloud token (provisioning stays locked until you add it in the dashboard).")
			return nil
		}
		cv, _ := parseJSON(cred)
		// `.data.validation.writable // "unknown"` — jq's `//` also fires on
		// a JSON false, so only the STRING "false" reaches this branch (a
		// bash quirk kept as-is; see the package report).
		if jstrOr(cv, "unknown", "data", "validation", "writable") == "false" {
			c.echo("kubehz: hcloud token connected, but it is READ-ONLY — dashboard provisioning stays locked. Use a Read & Write token for worker pools.")
		} else {
			c.echo("kubehz: hcloud token connected — dashboard provisioning is enabled.")
		}
	}
	return nil
}

// RegisterCluster ports kubehz::register_cluster: announce a PENDING cluster
// with the platform. Non-fatal: warns on failure and returns nil so
// provisioning continues without kubehz integration.
//
// Order: authenticated direct claim (KUBEHZ_TOKEN) → api-minted claim key
// (HCLOUD_TOKEN) → the legacy server-key fingerprint announce (no bearer).
func (c *Context) RegisterCluster(ctx context.Context, cfg *Config, domain, clusterYAML string) error {
	apiURL := cfg.APIURL

	// Defense in depth: the fingerprint travels on this URL; never over plain
	// HTTP. register_cluster is also reachable standalone, so re-assert.
	if err := c.requireHTTPS(apiURL, "spec.kubehz.apiUrl"); err != nil {
		return nil
	}

	if cfg.Access == "managed" {
		c.echo("kubehz: access 'managed' — management features activate in the dashboard once the cluster is claimed by a Supporter+ tenant.")
	}

	if c.getenv("KUBEHZ_TOKEN") != "" {
		if err := c.directClaim(ctx, cfg, domain, clusterYAML, apiURL); err == nil {
			return nil
		}
		c.warnf("kubehz direct claim failed for %s; falling back to the anonymous registration flow", domain)
	}

	if c.getenv("HCLOUD_TOKEN") != "" {
		if err := c.ensureClaimKey(ctx, domain, apiURL); err == nil {
			return nil
		}
		c.warnf("kubehz claim-key registration failed for %s; falling back to the legacy fingerprint announce", domain)
	}

	fingerprint, err := c.SSHFingerprint(ctx, clusterYAML)
	if err != nil {
		c.warnf("Could not extract SSH fingerprint for %s, skipping registration", domain)
		return nil
	}

	body, err := c.fetch(ctx, "POST", apiURL+"/api/clusters/register", bearer{},
		compactJSON(jsonPair{"domain", domain}, jsonPair{"fingerprint", fingerprint}))
	if err != nil {
		c.warnf("kubehz API request failed for %s, cluster will continue without kubehz integration", domain)
		return nil
	}
	v, _ := parseJSON(body)
	clusterID := jstrOr(v, "", "id")
	if clusterID == "" {
		c.warnf("kubehz registration returned no cluster id, cluster will continue without kubehz integration")
		return nil
	}

	c.debugf("Cluster registered with kubehz (pending claim): %s", clusterID)
	// The fingerprint is public (not a secret), so it is safe to print; the
	// Hetzner token is NEVER printed or sent here.
	c.echo("kubehz: cluster '%s' registered (pending). Claim it in the dashboard:", domain)
	c.echo("  fingerprint: %s", fingerprint)
	c.echo("kubehz: preferred — deploy the heartbeat agent, then claim with:")
	c.echo("  lo kubehz claim-code   # prints the code to paste at the dashboard /claim page")
	return nil
}

// errNotRegistered is resolve_cluster_id's rc 2: the registry answered, but
// carries no row for the domain.
var errNotRegistered = errors.New("kubehz: no cluster registered for domain")

// ResolveClusterID ports kubehz::resolve_cluster_id: the platform cluster
// id (cl-<uuid8>) for a domain from the tenant's registry. The list
// endpoint IGNORES ?domain=, so the rows are filtered CLIENT-SIDE,
// oldest-first (the server binds agent identity to the OLDEST active row).
// A failed list request is a plain error; a list without a row for the
// domain is errNotRegistered — deregister/destroy branch on the distinction
// so a lookup FAILURE is never reported as "not registered".
func (c *Context) ResolveClusterID(ctx context.Context, domain, apiURL string) (string, error) {
	body, err := c.fetch(ctx, "GET", apiURL+"/api/clusters?perPage=500", withBearer(c.getenv("KUBEHZ_TOKEN")), nil)
	if err != nil {
		return "", err
	}
	v, ok := parseJSON(body)
	if !ok {
		return "", errors.New("kubehz: registry list is not JSON")
	}
	clusterID := ""
	if rs := domainRows(v, domain); len(rs) > 0 {
		clusterID = jstrOr(rs[0], "", "id")
	}
	// A >500-cluster tenant pages past the cap; a truncated list must not
	// read as "not registered" silently.
	if clusterID == "" {
		if total := jnum(jget(v, "meta", "pagination", "total")); total > 500 {
			c.warnf("kubehz: tenant has %d clusters (first 500 checked) — the domain may sit past the page cap", total)
		}
	}
	if clusterID == "" {
		return "", errNotRegistered
	}
	return clusterID, nil
}

// DeregisterCluster ports kubehz::deregister_cluster: resolve the platform
// id for the domain, DELETE /api/clusters/<id>, then retire the hcloud claim
// key. Returns an error whenever REMOVAL FAILED (lookup error, refused
// delete); an already-absent row is idempotent success.
func (c *Context) DeregisterCluster(ctx context.Context, cfg *Config, domain, clusterYAML string) error {
	apiURL := cfg.APIURL
	var rc error

	// deregister skips validate_config (reachable standalone), so guard each
	// call by its own URL.
	if err := c.requireHTTPS(apiURL, "spec.kubehz.apiUrl"); err != nil {
		rc = ErrHandled
	} else {
		clusterID, err := c.ResolveClusterID(ctx, domain, apiURL)
		switch {
		case errors.Is(err, errNotRegistered):
			c.echo("kubehz: no cluster is registered for %s — nothing to remove on the platform.", domain)
		case err != nil:
			c.errorf("kubehz: cannot read the cluster registry at %s — the registration for %s was not removed. Set KUBEHZ_TOKEN to a clusters:write token of the owning tenant, then retry.", apiURL, domain)
			rc = ErrHandled
		default:
			// Status + body captured: a refused delete must surface with the
			// api's own message, not vanish into `curl -f`.
			res, err := c.fetchStatus(ctx, "DELETE", apiURL+"/api/clusters/"+clusterID, withBearer(c.getenv("KUBEHZ_TOKEN")), nil)
			switch {
			case err != nil:
				c.errorf("kubehz: DELETE /api/clusters/%s failed (network) — the registration for %s was not removed. Retry when %s is reachable.", clusterID, domain, apiURL)
				rc = ErrHandled
			case is2xx(res.Status):
				c.echo("kubehz: cluster %s (%s) removed from the platform.", domain, clusterID)
			default:
				msg := apiMessage(res.Body)
				c.errorf("kubehz: delete refused for %s (%s) — HTTP %d%s. The registration was not removed.", domain, clusterID, res.Status, optSuffix(": ", msg))
				rc = ErrHandled
			}
		}
	}

	// Best-effort: retire the kubehz claim key from the hcloud account. The
	// name mirrors the api's minted convention (kubehz-claim-<domain>).
	if hcloudToken := c.getenv("HCLOUD_TOKEN"); hcloudToken != "" {
		hcloudAPI := c.hcloudAPIBase()
		if err := c.requireHTTPS(hcloudAPI, "HCLOUD_API_BASE"); err == nil {
			auth := withBearer(hcloudToken)
			keyID := ""
			if raw, err := c.fetch(ctx, "GET", hcloudAPI+"/v1/ssh_keys?name="+url.QueryEscape("kubehz-claim-"+domain), auth, nil); err == nil {
				if lv, ok := parseJSON(raw); ok {
					if keys, ok := jget(lv, "ssh_keys").([]any); ok && len(keys) > 0 {
						keyID = jstrOr(keys[0], "", "id")
					}
				}
			}
			if keyID != "" {
				_, _ = c.fetch(ctx, "DELETE", hcloudAPI+"/v1/ssh_keys/"+keyID, auth, nil)
			}
		}
	}
	return rc
}

// apiMessage is `.data.message // .message // empty` over a body.
func apiMessage(body []byte) string {
	v, ok := parseJSON(body)
	if !ok {
		return ""
	}
	return jstr(jalt("", jget(v, "data", "message"), jget(v, "message")))
}

// apiHelp is `.data.help // .help // empty` over a body.
func apiHelp(body []byte) string {
	v, ok := parseJSON(body)
	if !ok {
		return ""
	}
	return jstr(jalt("", jget(v, "data", "help"), jget(v, "help")))
}

// apiCode is `.data.code // .code // empty` over a body.
func apiCode(body []byte) string {
	v, ok := parseJSON(body)
	if !ok {
		return ""
	}
	return jstr(jalt("", jget(v, "data", "code"), jget(v, "code")))
}

// optSuffix is bash's `${var:+<prefix>${var}}`.
func optSuffix(prefix, val string) string {
	if val == "" {
		return ""
	}
	return prefix + val
}

// jnum reads a JSON number as int (0 for anything else) — the
// `.meta.pagination.total // 0` shape.
func jnum(v any) int {
	if n, ok := v.(interface{ Int64() (int64, error) }); ok {
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
}
