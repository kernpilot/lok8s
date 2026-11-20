package kubehz

// hosted.go — libs/kubehz/hosted: creating/destroying clusters via the
// kubehz api for hosting: hosted (the flows the kubeone/capi drivers call
// through their Hooks).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/kernpilot/lok8s/internal/interrupt"
)

// buildClusterPayload ports kubehz::build_cluster_payload. `kind` is the
// WIRE field in the spec's ORIGINAL case ("KubeOne") — a cross-repo
// contract, never lowercased. controlPlaneReplicas rides as a JSON number
// (`--argjson`): a non-numeric scalar made jq fail and the payload EMPTY in
// bash; mirrored here as nil.
func buildClusterPayload(clusterYAML string) []byte {
	doc := loadSpec(clusterYAML)
	replicas := doc.Or("1", "spec", "controlPlane", "replicas")
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
	pairs := []jsonPair{
		{"domain", doc.Raw("spec", "cluster", "domain")},
		{"kind", doc.Raw("kind")},
		{"provider", doc.Or("hetzner", "spec", "provider")},
		{"region", doc.OrChain("fsn1", []string{"spec", "hcloud", "region"}, []string{"spec", "aws", "region"})},
	}
	// An unset version (kubernetesVersion: "") is left out of the payload.
	// The bare read gave the word "null", which went out as the version
	// string "null" (issue #272). Bash: the jq branch that adds the key
	// only when it is not empty.
	if v := doc.kubernetesVersion(); v != "" {
		pairs = append(pairs, jsonPair{"kubernetesVersion", v})
	}
	pairs = append(pairs,
		jsonPair{"controlPlaneReplicas", replicasJSON},
		jsonPair{"hosting", doc.Or("self", "spec", "kubehz", "hosting")},
		jsonPair{"access", doc.Or("none", "spec", "kubehz", "access")},
	)
	return compactJSON(pairs...)
}

// kubernetesVersion is spec.kubernetes.version, or "" when it is unset: no
// key, a YAML null, false or an empty string (yq's
// `.spec.kubernetes.version // ""`: `//` fires on null and on false). A
// number keeps its text ("0", "1.30"). The payload and the refusal in
// ProvisionHosted use this one read. Bash: kubehz::_hosted_version.
func (d specDoc) kubernetesVersion() string {
	return d.Or("", "spec", "kubernetes", "version")
}

// missingVersionLine is the refusal of a hosted spec with no
// spec.kubernetes.version (bash: kubehz::provision_hosted, the same text).
func missingVersionLine(clusterYAML string) string {
	return "kubehz: spec.kubernetes.version is not set in " + clusterYAML +
		". A hosted cluster needs a Kubernetes version, so no cluster was created. Set it, then run the command again."
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
		if err := c.sleep(ctx, 10*time.Second); err != nil {
			return err
		}
	}
	c.errorf("Timed out waiting for hosted cluster %s after %ds", clusterID, timeout)
	return ErrHandled
}

var digitsRe = regexp.MustCompile(`^[0-9]+$`)

// capacityDetail reads one `detail` field of the 503 envelope: the UI route
// nests it under `data`, the bare body does not. Absent → "". Every value
// here reaches a terminal, so it passes through the same scrub and clip the
// rest of the package applies to a server string: a control character could
// rewrite the screen, and a long one could bury the message.
func capacityDetail(v any, key string) string {
	return clip(scrub(jstr(jalt("", jget(v, "data", "detail", key), jget(v, "detail", key)))))
}

// renderCapacityRejection renders the api's 503 AT_CAPACITY envelope. It
// deviates from kubehz::render_capacity_rejection (D37): the frozen bash
// reads `detail.tier` and speaks of plans. Plans are gone. The api keys
// capacity on the control-plane SHAPE (kubehz-api server/domains/capacity)
// and `detail` carries one of three variants:
//
//   - the shape gate:    { replicas, used, limit, retryAfter }
//   - the metal gate:    { option: "dedicated", metalNodes, metalRequired, retryAfter }
//   - the free pool:     { pool: "free", used, cap, retryAfter }
//
// The gate counts apiserver replicas only (footprint = base + replicas ×
// per-replica requests); the state limit does not enter it. An older api
// still sends { tier, used, limit, retryAfter }: `tier` stays as a fallback
// subject, without the plan words. The "live capacity" pointer uses the
// module-global api URL (cfg.APIURL): GET /api/capacity lists used/limit
// per shape preset.
// slotUsage renders " (3/3 in use, 0 free)"; the free count only when both
// sides are numbers, nothing when one side is absent. A run of digits can
// still overflow an int, and Atoi then returns the clamped value with an
// error: print the counts the api sent and drop the subtraction.
func slotUsage(used, limit string) string {
	if used == "" || limit == "" {
		return ""
	}
	usage := " (" + used + "/" + limit + " in use"
	u, uErr := strconv.Atoi(used)
	l, lErr := strconv.Atoi(limit)
	if uErr == nil && lErr == nil && u >= 0 && l >= 0 {
		usage += ", " + strconv.Itoa(max(l-u, 0)) + " free"
	}
	return usage + ")"
}

func (c *Context) renderCapacityRejection(cfg *Config, body []byte) {
	v, _ := parseJSON(body)
	replicas := capacityDetail(v, "replicas")
	option := capacityDetail(v, "option")
	pool := capacityDetail(v, "pool")
	tier := capacityDetail(v, "tier")
	used := capacityDetail(v, "used")
	limit := capacityDetail(v, "limit")
	retry := capacityDetail(v, "retryAfter")

	// The subject of the headline, the count in parentheses and the hint
	// that fits it. The shape hint is the default; the option and pool
	// branches replace it.
	subject := "this shape"
	hint := "    • pick a smaller shape: fewer apiserver replicas (spec.controlPlane.replicas, 1 to 3)"
	usage := ""
	switch {
	case option != "":
		// The metal gate counts nodes present vs required, not slots: no
		// free count here.
		subject = "the '" + option + "' option"
		nodes := capacityDetail(v, "metalNodes")
		required := capacityDetail(v, "metalRequired")
		if nodes != "" && required != "" {
			usage = " (" + nodes + "/" + required + " metal nodes present)"
		}
		hint = "    • create the cluster without the '" + option + "' option now and turn it on later (it migrates live)"
	case pool != "":
		subject = "the " + pool + " hosted control-plane pool"
		usage = slotUsage(used, capacityDetail(v, "cap"))
		hint = "    • add a payment method: paid hosted control planes do not share this pool"
	case replicas != "":
		subject = "a control plane with " + replicas + " apiserver replica"
		if replicas != "1" {
			subject += "s"
		}
		usage = slotUsage(used, limit)
	case tier != "":
		// Fallback for an api that still keys capacity on a tier name.
		subject = "the '" + tier + "' shape"
		usage = slotUsage(used, limit)
	default:
		usage = slotUsage(used, limit)
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

	c.errorf("kubehz: the platform is at capacity for %s right now%s.", subject, usage)
	// Variant-neutral: only the free-pool gate is about a pool. The metal
	// gate counts nodes and the shape gate counts replica slots.
	c.echoErr("  You can:")
	c.echoErr("    • retry later: capacity frees when a cluster is deleted")
	if retryHint != "" {
		c.echoErr("    • suggested wait: %s", retryHint)
	}
	c.echoErr("%s", hint)
	c.echoErr("    • check the live capacity per shape and retry when a slot opens:")
	c.echoErr("        %s/api/capacity", cfg.APIURL)
	c.echoErr("    • run a self-hosted cluster meanwhile (spec.kubehz.hosting: self)")
	c.echoErr("    • still stuck? contact kubehz support with the shape + time above")
}

// ProvisionHosted ports kubehz::provision_hosted: create the cluster via
// POST /api/clusters, wait for it, download the kubeconfig to
// .kubeconfig/<domain>.yaml and mirror it to
// .kubeconfig/<metadata.name>.yaml for the dispatch tail. Both files are
// written by writePrivateFile (a 0600 temp file and one rename): a failed
// download or write leaves the old file as it was. The kubeconfig path is
// checked before the create: a path that the write refuses stops the
// provision there, so a refusal creates no cluster. A spec with no
// spec.kubernetes.version is refused first, before any request: the
// platform needs the version and refuses a create without it (HTTP 400,
// a missing field).
func (c *Context) ProvisionHosted(ctx context.Context, cfg *Config, domain, clusterYAML string) error {
	if loadSpec(clusterYAML).kubernetesVersion() == "" {
		c.errorf("%s", missingVersionLine(clusterYAML))
		return ErrHandled
	}
	apiURL := cfg.APIURL
	// bash: kubehz::_private_target before the POST.
	kubeconfigPath := filepath.Join(c.Paths.Base, ".kubeconfig", domain+".yaml")
	if _, err := privateTarget(kubeconfigPath); err != nil {
		c.errorf("%s", err)
		return ErrHandled
	}
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

	if err := os.MkdirAll(filepath.Dir(kubeconfigPath), 0o755); err != nil {
		return err
	}
	// A failed download leaves the old kubeconfig as it was: the file is
	// written only after the whole answer is here (bash: the download goes
	// into the temp file of kubehz::_write_private).
	kc, err := c.fetch(ctx, "GET", apiURL+"/api/clusters/"+clusterID+"/kubeconfig", withBearer(c.getenv("KUBEHZ_TOKEN")), nil)
	if err != nil {
		c.errorf("Failed to download kubeconfig for hosted cluster %s", clusterID)
		return ErrHandled
	}
	if err := writePrivateFile(ctx, kubeconfigPath, kc); err != nil {
		if refused, ok := errors.AsType[privateFileError](err); ok {
			c.errorf("%s", refused)
		}
		c.errorf("Failed to download kubeconfig for hosted cluster %s", clusterID)
		return ErrHandled
	}
	c.debugf("Hosted cluster %s ready, kubeconfig at %s", domain, kubeconfigPath)

	// The mirror is written in the same way, not onto the old file.
	clusterName := loadSpec(clusterYAML).Raw("metadata", "name")
	if clusterName != "" && clusterName != "null" && clusterName != domain {
		mirror := filepath.Join(c.Paths.Base, ".kubeconfig", clusterName+".yaml")
		if err := writePrivateFile(ctx, mirror, kc); err != nil {
			c.echoErr("[kubehz] warning: could not mirror the kubeconfig to .kubeconfig/%s.yaml — the bootstrap step will report it missing", clusterName)
		}
	}
	return nil
}

// privateFileError is a refusal of writePrivateFile; its text is the
// error line that bash prints (kubehz::_write_private).
type privateFileError string

func (e privateFileError) Error() string { return string(e) }

// writePrivateFile ports kubehz::_write_private: data goes into a new
// temp file beside the file (os.CreateTemp: mode 0600, the name
// <file>.<random>), and one rename replaces the file. So dest is the old
// file or the complete new one, never a part, and never readable by
// others. A failed write or rename leaves the old file as it was, and the
// deferred removal deletes the temp file on each return with an error.
// A dest that is a symlink is resolved (privateTarget): the temp file goes
// beside the file that the link names, and the rename replaces that file,
// so the link stays. A link to nothing, a link that cannot be resolved and
// a directory are refused (privateFileError). os.Rename is a rename onto
// the exact path: it fails when another operation put a directory there
// during the download (it never moves the file into it, as a mv without
// -T does), so bash's check after the rename has no twin here. A failed
// rename is a privateFileError with the text of bash.
// Signals: INT and TERM (interrupt.Watch) and HUP (interrupt.CatchHangup,
// for the time of the call) cancel the contexts and do not stop lo in the
// middle of this function: the write ends, and the rename or the deferred
// removal takes the temp file. Bash removes the temp file on such a signal
// and keeps the old file: a deliberate deviation (catalogue D45). SIGKILL cannot be caught: in the short time
// between CreateTemp and Rename it leaves the temp file (mode 0600).
func writePrivateFile(ctx context.Context, dest string, data []byte) (err error) {
	_, stopHangup := interrupt.CatchHangup(ctx)
	defer stopHangup()
	target, err := privateTarget(dest)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".*")
	if err != nil {
		return privateFileError("Cannot create a temp file beside " + target)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = renameFile(tmp.Name(), target); err != nil {
		if info, serr := os.Stat(target); serr == nil && info.IsDir() {
			return privateFileError(dest + " is a directory, not a file")
		}
		return privateFileError("Cannot rename the temp file to " + target)
	}
	return nil
}

// renameFile is os.Rename. A test replaces it, to make the rename fail
// after the temp file exists.
var renameFile = os.Rename

// privateTarget ports kubehz::_private_target: the file that a write of
// dest replaces (dest, or for a symlink the file that it names). A link
// to nothing, a link that cannot be resolved and a directory are refused
// (privateFileError, the text of bash).
func privateTarget(dest string) (string, error) {
	target := dest
	info, lerr := os.Lstat(dest)
	if lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		var err error
		if target, err = filepath.EvalSymlinks(dest); err != nil {
			return "", privateFileError("Cannot resolve the symlink " + dest)
		}
		info, lerr = os.Stat(target)
	}
	if lerr == nil && info.IsDir() {
		return "", privateFileError(dest + " is a directory, not a file")
	}
	return target, nil
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
	if err != nil && !errors.Is(err, errNotRegistered) {
		c.errorf("kubehz: cannot read the cluster registry at %s — nothing was deleted for %s and the local kubeconfigs are kept. Set KUBEHZ_TOKEN to a clusters:write token of the owning tenant, then retry.", apiURL, domain)
		return ErrHandled
	}
	if errors.Is(err, errNotRegistered) {
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
	mirrorName := loadSpec(clusterYAML).Or("", "metadata", "name")
	if mirrorName != "" && mirrorName != "null" && mirrorName != domain {
		_ = os.Remove(filepath.Join(c.Paths.Base, ".kubeconfig", mirrorName+".yaml"))
	}
	return nil
}
