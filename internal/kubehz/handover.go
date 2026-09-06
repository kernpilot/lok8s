package kubehz

// handover.go — libs/kubehz/handover: the eject TARGET side of a
// control-plane handover (the cross-repo handover contract, §4 lok8s scope).
//
// `receive` restores an exported hosted control plane onto THE NODE IT RUNS
// ON: seed the exported PKI into /etc/kubernetes/pki, restore the etcd
// snapshot into /var/lib/etcd, then `kubeadm init` against the pre-seeded
// state (kubeadm REUSES existing ca/sa/front-proxy files — that is what
// preserves the cluster identity). `preseed` only places the PKI on a
// kubeone node0 over SSH before `kubeone apply`.
//
// Test seams (env): KUBEHZ_HANDOVER_K8S_DIR, KUBEHZ_HANDOVER_ETCD_DIR,
// KUBEHZ_HANDOVER_WAIT, KUBEHZ_HANDOVER_ETCD_IMAGE_TAG.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The contract §2 bundle keys. The six PKI files are what preseed places;
// receive additionally needs the restore artifacts.
var (
	handoverPKIKeys    = []string{"ca.crt", "ca.key", "sa.pub", "sa.key", "front-proxy-ca.crt", "front-proxy-ca.key"}
	handoverBundleKeys = append(append([]string{}, handoverPKIKeys...), "encryption-key", "snapshot-location", "endpoint-dns")
)

// ReceiveOpts carries the `handover receive` flags.
type ReceiveOpts struct {
	Bundle     string
	Snapshot   string
	SingleNode bool
	Force      bool
}

// PreseedOpts carries the `handover preseed` flags.
type PreseedOpts struct {
	Bundle string
	Node   string
	User   string
	Port   int
	SSHKey string
}

// resolveBundle ports handover::resolve_bundle: a directory is used as-is;
// a .tar.gz is unpacked into a private (0700) dir inside workdir (or a
// fresh temp dir the caller owns) after a path-traversal check on every
// entry.
func (c *Context) resolveBundle(bundle, workdir string) (string, error) {
	if info, err := os.Stat(bundle); err == nil && info.IsDir() {
		return bundle, nil
	}
	if fileExists(bundle) {
		var dir string
		if workdir != "" {
			dir = filepath.Join(workdir, "bundle")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return "", err
			}
		} else {
			d, err := os.MkdirTemp("", "")
			if err != nil {
				return "", err
			}
			dir = d
		}
		_ = os.Chmod(dir, 0o700) // #nosec G302 -- a directory: 0700 is owner-only
		if err := c.extractBundle(bundle, dir); err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
		return dir, nil
	}
	c.errorf("handover: bundle not found: %s", bundle)
	return "", ErrHandled
}

// bundleEntryEscapes is the traversal guard's truth table: absolute paths
// and a `..` COMPONENT in any position (sole, leading, interior, trailing).
func bundleEntryEscapes(entry string) bool {
	if strings.HasPrefix(entry, "/") || entry == ".." {
		return true
	}
	if strings.HasPrefix(entry, "../") || strings.HasSuffix(entry, "/..") || strings.Contains(entry, "/../") {
		return true
	}
	return false
}

// extractBundle lists, guards, then extracts a .tar.gz (bash: `tar -tzf`
// over every entry, then `tar -xzf`).
func (c *Context) extractBundle(bundle, dir string) error {
	f, err := os.Open(bundle)
	if err != nil {
		return c.notArchive(bundle)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return c.notArchive(bundle)
	}
	tr := tar.NewReader(gz)
	type entry struct {
		hdr  *tar.Header
		data []byte
	}
	var entries []entry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return c.notArchive(bundle)
		}
		if bundleEntryEscapes(hdr.Name) {
			c.errorf("handover: refusing %s — archive entry escapes the bundle dir: %s", bundle, hdr.Name)
			return ErrHandled
		}
		var data []byte
		if hdr.Typeflag == tar.TypeReg {
			data, err = io.ReadAll(tr)
			if err != nil {
				return c.notArchive(bundle)
			}
		}
		entries = append(entries, entry{hdr, data})
	}
	for _, e := range entries {
		target := filepath.Join(dir, filepath.Clean("/"+e.hdr.Name))
		switch e.hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			// #nosec G115 -- the conversion happens first; only the low 9 bits
			// survive the &0o777 mask, so a wrapped value cannot widen the mode.
			if err := os.WriteFile(target, e.data, fs.FileMode(e.hdr.Mode)&0o777|0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Context) notArchive(bundle string) error {
	c.errorf("handover: %s is neither a bundle directory nor a readable .tar.gz archive", bundle)
	return ErrHandled
}

// validateBundle ports handover::validate_bundle: every contract §2 key must
// exist and be non-empty; the error NAMES the missing key.
func (c *Context) validateBundle(dir string) error {
	for _, key := range handoverBundleKeys {
		info, err := os.Stat(filepath.Join(dir, key))
		if err != nil || info.IsDir() || info.Size() == 0 {
			c.errorf("handover: bundle is incomplete — missing or empty key: %s (expected at %s/%s)", key, dir, key)
			return ErrHandled
		}
	}
	return nil
}

// placePKI ports handover::place_pki: seed the exported PKI into
// <k8sDir>/pki (kubeadm reuses EXISTING files instead of minting new ones).
func (c *Context) placePKI(bundle, k8sDir string) error {
	if err := os.MkdirAll(filepath.Join(k8sDir, "pki"), 0o755); err != nil {
		return err
	}
	for _, key := range handoverPKIKeys {
		if err := copyFile(filepath.Join(bundle, key), filepath.Join(k8sDir, "pki", key)); err != nil {
			c.errorf("handover: could not place %s into %s/pki", key, k8sDir)
			return ErrHandled
		}
	}
	for _, f := range []string{"ca.crt", "front-proxy-ca.crt", "sa.pub"} {
		_ = os.Chmod(filepath.Join(k8sDir, "pki", f), 0o644) // #nosec G302 -- the PUBLIC halves (certs, sa.pub); the keys stay 0600
	}
	for _, f := range []string{"ca.key", "front-proxy-ca.key", "sa.key"} {
		_ = os.Chmod(filepath.Join(k8sDir, "pki", f), 0o600)
	}
	return nil
}

// copyFile writes dst private (0600) from the first byte: the PKI copies
// include CA and service-account KEYS, and a 0644 create followed by a
// chmod leaves a readable window. placePKI widens the public halves after.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

// stripQuery drops everything from '?' on — for URLs that carry a credential
// (presigned object-store links) and must not be echoed whole.
func stripQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

// fetchSnapshot ports handover::fetch_snapshot: an explicit --snapshot wins;
// otherwise the bundle's snapshot-location is fetched BEST-EFFORT into the
// private workdir (0600). Every failure names the fix.
func (c *Context) fetchSnapshot(ctx context.Context, bundle, override, workdir string) (string, error) {
	if override != "" {
		if !fileExists(override) {
			c.errorf("handover: --snapshot file not found: %s", override)
			return "", ErrHandled
		}
		return override, nil
	}
	if workdir == "" {
		d, err := os.MkdirTemp("", "")
		if err != nil {
			return "", err
		}
		_ = os.Chmod(d, 0o700) // #nosec G302 -- a directory: 0700 is owner-only
		workdir = d
	}
	out := filepath.Join(workdir, "kubehz-handover-snapshot.db")
	raw, err := os.ReadFile(filepath.Join(bundle, "snapshot-location"))
	if err != nil {
		return "", err
	}
	location := trimNL(string(raw))
	switch {
	case strings.HasPrefix(location, "kkp://"):
		c.errorf("handover: snapshot-location '%s' is a KKP-internal locator (kkp://<destination>/<clusterID>/<backupName>) — it cannot be fetched from this node. Obtain the snapshot object via the platform (the tier2/api bundle download provides it) and re-run with --snapshot <file>", location)
		return "", ErrHandled
	case strings.HasPrefix(location, "s3://"):
		if !c.lookPath("aws") {
			c.errorf("handover: the snapshot lives at %s but no aws CLI is on this node — download it with any s3 client against the platform's object store and re-run with --snapshot <file>", location)
			return "", ErrHandled
		}
		if err := touch600(out); err != nil {
			return "", err
		}
		if _, err := c.capture(ctx, false, "aws", "s3", "cp", location, out); err != nil {
			c.errorf("handover: fetching %s via the aws CLI failed — download it yourself and re-run with --snapshot <file>", location)
			return "", ErrHandled
		}
		_ = os.Chmod(out, 0o600)
	case strings.HasPrefix(location, "https://"):
		if err := touch600(out); err != nil {
			return "", err
		}
		data, err := c.fetch(ctx, "GET", location, bearer{}, nil)
		if err != nil {
			// A presigned URL carries its credential in the query string —
			// name the object, not the token.
			c.errorf("handover: downloading the snapshot from %s failed — download it yourself and re-run with --snapshot <file>", stripQuery(location))
			return "", ErrHandled
		}
		if err := os.WriteFile(out, data, 0o600); err != nil {
			return "", err
		}
	case strings.HasPrefix(location, "/"):
		if !fileExists(location) {
			c.errorf("handover: snapshot-location points at %s, which does not exist on this node — re-run with --snapshot <file>", location)
			return "", ErrHandled
		}
		out = location
	default:
		c.errorf("handover: unsupported snapshot-location '%s' (expected s3://, https:// or an absolute path) — re-run with --snapshot <file>", location)
		return "", ErrHandled
	}
	return out, nil
}

// touch600 is `: > out && chmod 600 out` — 0600 BEFORE any bytes land.
func touch600(path string) error {
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// memberIdentity ports handover::member_identity: the etcd member name +
// peer URL kubeadm's static pod will start with (lowercased hostname; the
// default-route source address).
func (c *Context) memberIdentity(ctx context.Context) (string, string, error) {
	host, err := c.hostname()
	if err != nil {
		host = ""
	}
	nodeName := strings.ToLower(trimNL(host))
	nodeIP := ""
	if out, err := c.capture(ctx, true, "ip", "-4", "route", "get", "1.1.1.1"); err == nil {
		nodeIP = routeSrc(out)
	}
	peerURL := ""
	if nodeIP != "" {
		peerURL = "https://" + nodeIP + ":2380"
	} else {
		if out, err := c.capture(ctx, true, "ip", "-6", "route", "get", "2606:4700:4700::1111"); err == nil {
			nodeIP = routeSrc(out)
		}
		if nodeIP != "" {
			peerURL = "https://[" + nodeIP + "]:2380"
		}
	}
	if nodeIP == "" {
		c.errorf("handover: cannot determine this node's advertise address (no default route?) — etcd's member peer URL must match what kubeadm advertises; fix routing and re-run.")
		return "", "", ErrHandled
	}
	return nodeName, peerURL, nil
}

// routeSrc is `awk '{for(i=1;i<NF;i++) if($i=="src"){print $(i+1); exit}}'`.
func routeSrc(out string) string {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "src" {
				return f[i+1]
			}
		}
	}
	return ""
}

// restoreSnapshot ports handover::restore_snapshot: etcdutl, with etcdctl
// (ETCDCTL_API=3) as the fallback; --skip-hash-check (KKP snapshots are
// streamed, no trailer) and the member identity rewritten to what kubeadm
// will start with.
func (c *Context) restoreSnapshot(ctx context.Context, snapshot, etcdDir, nodeName, peerURL string) error {
	if entries, err := os.ReadDir(etcdDir); err == nil && len(entries) > 0 {
		c.errorf("handover: %s is not empty — refusing to restore over existing etcd data", etcdDir)
		return ErrHandled
	}
	_ = os.Remove(etcdDir)
	args := []string{"snapshot", "restore", snapshot, "--data-dir", etcdDir, "--skip-hash-check=true",
		"--name", nodeName,
		"--initial-cluster", nodeName + "=" + peerURL,
		"--initial-advertise-peer-urls", peerURL}
	switch {
	case c.lookPath("etcdutl"):
		return c.run(ctx, "etcdutl", args...)
	case c.lookPath("etcdctl"):
		return c.Runner.Run(ctx, execxCmdEnv("etcdctl", args, []string{"ETCDCTL_API=3"}, c))
	default:
		c.errorf("handover: neither etcdutl nor etcdctl found on this node — install the etcd client tooling and re-run")
		return ErrHandled
	}
}

// bundleValue ports handover::bundle_value: an OPTIONAL bundle file,
// surrounding whitespace trimmed, missing/blank → def.
func bundleValue(bundle, key, def string) string {
	raw, err := os.ReadFile(filepath.Join(bundle, key))
	if err != nil {
		return def
	}
	v := strings.TrimSpace(string(raw))
	if v == "" {
		return def
	}
	return v
}

var (
	b64Re         = regexp.MustCompile(`^[A-Za-z0-9+/=]+$`)
	encKeyNameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)
	hostnameRe    = regexp.MustCompile(`^[a-zA-Z0-9.-]+$`)
	imageTagRe    = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	handoverPKIRe = regexp.MustCompile(`\.key$`)
)

// encryptionKey ports handover::encryption_key: read + VALIDATE the base64
// key (edges trimmed; interior whitespace fails the charset gate).
func (c *Context) encryptionKey(bundle string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(bundle, "encryption-key"))
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(raw))
	if !b64Re.MatchString(key) {
		c.errorf("handover: bundle encryption-key is not base64 — refusing to render the EncryptionConfiguration")
		return "", ErrHandled
	}
	return key, nil
}

// encryptionMetadata ports handover::encryption_metadata: the provider
// (allowlisted) and key name (plain-name charset).
func (c *Context) encryptionMetadata(bundle string) (string, string, error) {
	provider := bundleValue(bundle, "encryption-provider", "secretbox")
	keyName := bundleValue(bundle, "encryption-key-name", "kubehz-key-1")
	switch provider {
	case "secretbox", "aescbc", "aesgcm":
	default:
		c.errorf("handover: bundle encryption-provider '%s' is not one of secretbox/aescbc/aesgcm", head(provider, 64))
		return "", "", ErrHandled
	}
	if !encKeyNameRe.MatchString(keyName) {
		c.errorf("handover: bundle encryption-key-name '%s' is not a plain name", head(keyName, 64))
		return "", "", ErrHandled
	}
	return provider, keyName, nil
}

// head is bash `${var:0:n}`.
func head(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// writeEncryptionConfig ports handover::write_encryption_config (0600: the
// file embeds key material). Returns the file path.
func (c *Context) writeEncryptionConfig(bundle, k8sDir string) (string, error) {
	out := filepath.Join(k8sDir, "kubehz-encryption-config.yaml")
	key, err := c.encryptionKey(bundle)
	if err != nil {
		return "", err
	}
	provider, keyName, err := c.encryptionMetadata(bundle)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(k8sDir, 0o755); err != nil {
		return "", err
	}
	content := "apiVersion: apiserver.config.k8s.io/v1\n" +
		"kind: EncryptionConfiguration\n" +
		"resources:\n" +
		"  - resources:\n" +
		"      - secrets\n" +
		"    providers:\n" +
		"      - " + provider + ":\n" +
		"          keys:\n" +
		"            - name: " + keyName + "\n" +
		"              secret: " + key + "\n" +
		"      - identity: {}\n"
	if err := os.WriteFile(out, []byte(content), 0o600); err != nil {
		return "", err
	}
	if err := os.Chmod(out, 0o600); err != nil {
		return "", err
	}
	return out, nil
}

// endpointDNS ports handover::endpoint_dns: the ONE validated reader of the
// bundle's endpoint DNS (interpolated into YAML AND passed to kubectl).
func (c *Context) endpointDNS(bundle string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(bundle, "endpoint-dns"))
	if err != nil {
		return "", err
	}
	dns := strings.TrimSpace(string(raw))
	if !hostnameRe.MatchString(dns) {
		c.errorf("handover: bundle endpoint-dns '%s' is not a plain hostname", dns)
		return "", ErrHandled
	}
	return dns, nil
}

// writeKubeadmConfig ports handover::write_kubeadm_config (v1beta4).
func (c *Context) writeKubeadmConfig(bundle, k8sDir, out, etcdTag string) error {
	enc := filepath.Join(k8sDir, "kubehz-encryption-config.yaml")
	dns, err := c.endpointDNS(bundle)
	if err != nil {
		return err
	}
	content := "apiVersion: kubeadm.k8s.io/v1beta4\n" +
		"kind: ClusterConfiguration\n" +
		"controlPlaneEndpoint: \"" + dns + ":6443\"\n" +
		"etcd:\n" +
		"  local:\n" +
		"    imageTag: \"" + etcdTag + "\"\n" +
		"apiServer:\n" +
		"  certSANs:\n" +
		"    - \"" + dns + "\"\n" +
		"  extraArgs:\n" +
		"    - name: encryption-provider-config\n" +
		"      value: " + enc + "\n" +
		"  extraVolumes:\n" +
		"    - name: kubehz-encryption-config\n" +
		"      hostPath: " + enc + "\n" +
		"      mountPath: " + enc + "\n" +
		"      readOnly: true\n" +
		"      pathType: File\n"
	return os.WriteFile(out, []byte(content), 0o644) // #nosec G306 -- a static-pod manifest; the secret it mounts is its own 0600 file
}

// kubeadmInit ports handover::kubeadm_init.
func (c *Context) kubeadmInit(ctx context.Context, config string) error {
	return c.run(ctx, "kubeadm", "init", "--config", config,
		"--ignore-preflight-errors=DirAvailable--var-lib-etcd",
		"--skip-phases=addon/coredns,addon/kube-proxy")
}

// verify ports handover::verify: the apiserver answers WITH the restored
// data (probed on its LOCAL address with the endpoint DNS as TLS server
// name), and the live CA is byte-identical to the bundle's.
func (c *Context) verify(ctx context.Context, bundle, k8sDir string) error {
	kubeconfig := filepath.Join(k8sDir, "admin.conf")
	timeout := 300
	if v := c.getenv("KUBEHZ_HANDOVER_WAIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			timeout = n
		}
	}
	localAPI := "https://127.0.0.1:6443"
	serverName, err := c.endpointDNS(bundle)
	if err != nil {
		return err
	}
	waited := 0
	for {
		if err := c.runQuiet(ctx, "kubectl", "--kubeconfig", kubeconfig, "--server", localAPI,
			"--tls-server-name", serverName, "get", "ns", "kube-system"); err == nil {
			break
		}
		if waited >= timeout {
			c.errorf("handover: the apiserver did not become ready within %ds (kubectl --kubeconfig %s --server %s --tls-server-name %s get ns kube-system kept failing) — state left behind: %s/pki holds the bundle PKI and the restored etcd data dir may be corrupt (a truncated snapshot restores 'successfully' under --skip-hash-check and only fails here). Clean up ('kubeadm reset') before retrying.", timeout, kubeconfig, localAPI, serverName, k8sDir)
			return ErrHandled
		}
		c.sleep(5 * time.Second)
		waited += 5
	}
	want, err := fileSHA256(filepath.Join(bundle, "ca.crt"))
	if err != nil {
		return err
	}
	got, err := fileSHA256(filepath.Join(k8sDir, "pki", "ca.crt"))
	if err != nil {
		return err
	}
	if want != got {
		c.errorf("handover: CA hash mismatch after kubeadm init — %s/pki/ca.crt is NOT the bundle's CA (identity was not preserved; do NOT cut over)", k8sDir)
		return ErrHandled
	}
	c.echo("handover: verified — apiserver is up and the cluster CA matches the export bundle (sha256 %s)", got)
	return nil
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// firstRealContent is `find <dir> ! -type d -print -quit`: ANY non-directory
// entry is content. A traversal error fails CLOSED.
func firstRealContent(dir string) (string, error) {
	found := ""
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	return found, walkErr
}

// HandoverReceive ports handover::receive — RUNS ON THE TARGET NODE.
func (c *Context) HandoverReceive(ctx context.Context, o ReceiveOpts) error {
	k8sDir := c.getenv("KUBEHZ_HANDOVER_K8S_DIR")
	if k8sDir == "" {
		k8sDir = "/etc/kubernetes"
	}
	etcdDir := c.getenv("KUBEHZ_HANDOVER_ETCD_DIR")
	if etcdDir == "" {
		etcdDir = "/var/lib/etcd"
	}

	// Private scratch (0700) for the bundle extraction + a downloaded
	// snapshot, scrubbed on EVERY exit path.
	workdir, err := os.MkdirTemp("", "")
	if err != nil {
		return err
	}
	_ = os.Chmod(workdir, 0o700) // #nosec G302 -- a directory: 0700 is owner-only
	defer func() { _ = os.RemoveAll(workdir) }()

	bundleDir, err := c.resolveBundle(o.Bundle, workdir)
	if err != nil {
		return err
	}
	if err := c.validateBundle(bundleDir); err != nil {
		return err
	}

	// "not empty" means REAL content: kubeadm's package skeleton dirs
	// (manifests/, pki/) never block.
	if info, err := os.Stat(k8sDir); err == nil && info.IsDir() {
		real, findErr := firstRealContent(k8sDir)
		if findErr != nil && real == "" && !o.Force {
			c.errorf("handover: cannot inspect %s (find exit %d) — refusing to assume it is empty; fix permissions or re-run with --force.", k8sDir, 1)
			return ErrHandled
		}
		if real != "" && !o.Force {
			c.errorf("handover: %s holds existing content (%s) — either this node already runs (or ran) Kubernetes, or a previous receive attempt left its seeded state behind. Clean up first ('kubeadm reset' clears the seeded PKI and etcd data), then re-run — with --force if you really mean to overwrite what remains.", k8sDir, real)
			return ErrHandled
		}
	}
	if entries, err := os.ReadDir(etcdDir); err == nil && len(entries) > 0 {
		if o.Force {
			_ = os.RemoveAll(etcdDir)
		} else {
			c.errorf("handover: %s is not empty — refusing to restore over existing etcd data (re-run with --force to overwrite).", etcdDir)
			return ErrHandled
		}
	}

	// Fail before touching the node: snapshot, member identity, etcd pin,
	// and EVERY bundle-derived value first.
	snapshotFile, err := c.fetchSnapshot(ctx, bundleDir, o.Snapshot, workdir)
	if err != nil {
		return err
	}
	nodeName, peerURL, err := c.memberIdentity(ctx)
	if err != nil {
		return err
	}
	etcdTag := c.getenv("KUBEHZ_HANDOVER_ETCD_IMAGE_TAG")
	bundleTag := bundleValue(bundleDir, "etcd-version", "")
	if etcdTag == "" {
		etcdTag = bundleTag
		if etcdTag == "" {
			etcdTag = "3.5.21-0"
		}
	} else if bundleTag != "" && bundleTag != etcdTag {
		c.warnf("handover: KUBEHZ_HANDOVER_ETCD_IMAGE_TAG '%s' overrides the bundle's etcd-version '%s'", etcdTag, bundleTag)
	}
	if !imageTagRe.MatchString(etcdTag) {
		c.errorf("handover: etcd image tag '%s' is not a plain image tag — refusing before touching the node", head(etcdTag, 64))
		return ErrHandled
	}
	if _, _, err := c.encryptionMetadata(bundleDir); err != nil {
		return err
	}
	if _, err := c.encryptionKey(bundleDir); err != nil {
		return err
	}
	endpoint, err := c.endpointDNS(bundleDir)
	if err != nil {
		return err
	}

	if err := c.placePKI(bundleDir, k8sDir); err != nil {
		return err
	}
	if err := c.restoreSnapshot(ctx, snapshotFile, etcdDir, nodeName, peerURL); err != nil {
		return err
	}
	if _, err := c.writeEncryptionConfig(bundleDir, k8sDir); err != nil {
		return err
	}
	kubeadmConfig := filepath.Join(k8sDir, "kubehz-handover-kubeadm.yaml")
	if err := c.writeKubeadmConfig(bundleDir, k8sDir, kubeadmConfig, etcdTag); err != nil {
		return err
	}
	if err := c.kubeadmInit(ctx, kubeadmConfig); err != nil {
		c.errorf("handover: kubeadm init FAILED after the node was seeded — state left behind: %s/pki holds the bundle PKI and %s holds the restored snapshot. Clean up first ('kubeadm reset' removes both, or delete %s and %s yourself), then re-run receive with --force.", k8sDir, etcdDir, k8sDir, etcdDir)
		return ErrHandled
	}
	if err := c.verify(ctx, bundleDir, k8sDir); err != nil {
		return err
	}

	if o.SingleNode {
		if err := c.runQuiet(ctx, "kubectl", "--kubeconfig", filepath.Join(k8sDir, "admin.conf"),
			"--server", "https://127.0.0.1:6443", "--tls-server-name", endpoint,
			"taint", "nodes", "--all", "node-role.kubernetes.io/control-plane-"); err != nil {
			c.warnf("handover: could not untaint the control-plane node — run: kubectl --server https://127.0.0.1:6443 --tls-server-name %s taint nodes --all node-role.kubernetes.io/control-plane-", endpoint)
		}
	}

	c.echo("handover: receive complete. Next steps:")
	c.echo("  1. point the endpoint DNS (%s) at this node", endpoint)
	c.echo("  2. ack the cutover in the kubehz dashboard (the platform waits for a target-side heartbeat)")
	c.echo("  3. join workers / additional control planes as needed")
	return nil
}

// HandoverPreseed ports handover::preseed — the thin kubeone variant: place
// the six PKI files on node0 over SSH before `kubeone apply`.
func (c *Context) HandoverPreseed(ctx context.Context, o PreseedOpts) error {
	user := o.User
	if user == "" {
		user = "root"
	}
	port := o.Port
	if port == 0 {
		port = 22
	}
	workdir, err := os.MkdirTemp("", "")
	if err != nil {
		return err
	}
	_ = os.Chmod(workdir, 0o700) // #nosec G302 -- a directory: 0700 is owner-only
	defer func() { _ = os.RemoveAll(workdir) }()

	bundleDir, err := c.resolveBundle(o.Bundle, workdir)
	if err != nil {
		return err
	}
	if err := c.validateBundle(bundleDir); err != nil {
		return err
	}

	sshOpts := []string{
		"-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes", "-o", "LogLevel=ERROR", "-o", "ConnectTimeout=10",
	}
	if o.SSHKey != "" {
		sshOpts = append(sshOpts, "-i", o.SSHKey)
	}
	portStr := strconv.Itoa(port)
	target := user + "@" + o.Node

	ssh := func(remote string) error {
		args := append(append([]string{}, sshOpts...), "-p", portStr, target, remote)
		return c.run(ctx, "ssh", args...)
	}
	// umask 077 → the pki dir is 0700 from the start.
	if err := ssh("umask 077 && mkdir -p /etc/kubernetes/pki"); err != nil {
		c.errorf("handover: cannot reach %s over SSH", target)
		return ErrHandled
	}
	// Per-file transfer with an IMMEDIATE per-key chmod.
	for _, key := range handoverPKIKeys {
		args := append(append([]string{}, sshOpts...), "-P", portStr, filepath.Join(bundleDir, key), target+":/etc/kubernetes/pki/"+key)
		if err := c.run(ctx, "scp", args...); err != nil {
			c.errorf("handover: copying %s to %s failed", key, o.Node)
			return ErrHandled
		}
		if handoverPKIRe.MatchString(key) {
			if err := ssh("chmod 600 /etc/kubernetes/pki/" + key); err != nil {
				c.errorf("handover: could not chmod 600 /etc/kubernetes/pki/%s on %s — key material must not stay readable; fix the mode and re-run", key, o.Node)
				return ErrHandled
			}
		}
	}
	c.echo("handover: PKI pre-seeded on %s. Provision now (lo provision) — kubeone's kubeadm reuses the existing CA files, preserving cluster identity.", o.Node)
	return nil
}
