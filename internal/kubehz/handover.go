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
	"fmt"
	"github.com/kernpilot/lok8s/internal/execx"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/fsutil"
)

// The contract §2 bundle keys. The six PKI files are what preseed places;
// receive additionally needs the restore artifacts.
var (
	handoverPKIKeys    = []string{"ca.crt", "ca.key", "sa.pub", "sa.key", "front-proxy-ca.crt", "front-proxy-ca.key"}
	handoverBundleKeys = append(append([]string{}, handoverPKIKeys...), "encryption-key", "snapshot-location", "endpoint-dns")
	// The optional keys: read when they are there (bundleValue).
	handoverOptionalKeys = []string{"encryption-provider", "encryption-key-name", "etcd-version"}
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
	// KnownHosts is the known_hosts file ssh and scp check the node's host
	// key against ("" = ssh's own files). It must be an existing regular
	// file. An unknown or a changed key fails (StrictHostKeyChecking=yes).
	KnownHosts string
	// AcceptNewHostKey trusts an unknown host key on the first call only
	// (accept-new), then checks that ssh recorded it, and every later call
	// checks against that record.
	AcceptNewHostKey bool
}

// resolveBundle ports handover::resolve_bundle: a directory is used as-is;
// a .tar.gz is unpacked into a private (0700) dir inside workdir (or a
// fresh temp dir the caller owns) after a path-traversal check on every
// entry.
func (c *Context) resolveBundle(bundle, workdir string) (string, error) {
	if info, err := os.Stat(bundle); err == nil && info.IsDir() {
		return bundle, nil
	}
	if fsutil.FileExists(bundle) {
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
		if err := c.makePrivate(dir); err != nil {
			c.removeDir(dir)
			return "", err
		}
		if err := c.extractBundle(bundle, dir); err != nil {
			c.removeDir(dir)
			return "", err
		}
		return dir, nil
	}
	c.errorf("handover: bundle not found: %s", bundle)
	return "", ErrHandled
}

// removeDir ports handover::remove_dir: remove a dir that can hold the
// bundle's keys, and name it when that fails. The bash twin first gives
// the owner rwx on each dir in it (a tar can leave a dir 0400 or 0500);
// extractBundle makes every dir 0700, so the Go twin needs no such step.
func (c *Context) removeDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		c.errorf("handover: could not remove %s, which can hold the bundle's keys. Delete it yourself.", dir)
	}
}

// makePrivate ports handover::make_private: set dir to 0700, then check
// the mode it has. The bundle's keys land in it, so a failed chmod or a
// file system that keeps group or other bits fails closed.
func (c *Context) makePrivate(dir string) error {
	if err := c.chmod(dir, 0o700); err != nil { // #nosec G302 -- a directory: 0700 is owner-only
		c.errorf("handover: cannot set %s to mode 700, so the bundle's keys would not be private in it. Check the file system of TMPDIR, then retry.", dir)
		return ErrHandled
	}
	info, err := c.lstat(dir)
	if err == nil && info.IsDir() && info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	mode := "unknown"
	if err == nil {
		mode = strconv.FormatUint(uint64(info.Mode().Perm()), 8)
	}
	c.errorf("handover: %s has mode %s, not 700, so other users could read the bundle's keys in it. Check the archive and the file system of TMPDIR, then retry.", dir, mode)
	return ErrHandled
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
// and `tar -tvzf` over every entry, then `tar -xzf`). Two passes over the
// file: the first reads headers only, the second streams each file to
// disk, so the archive is never held in memory.
//
// Only regular files and directories: a link entry can name a file
// outside the bundle (ca.key -> /etc/shadow), and preseed would send that
// file to the node. Any other entry type refuses the whole bundle before
// a byte is written. A path that escapes is refused at its entry, as the
// bash traversal check reads every name before it reads the types.
func (c *Context) extractBundle(bundle, dir string) error {
	odd := false
	guard := func(_ *tar.Reader, hdr *tar.Header) error {
		if bundleEntryEscapes(hdr.Name) {
			c.errorf("handover: refusing %s — archive entry escapes the bundle dir: %s", bundle, hdr.Name)
			return ErrHandled
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir:
		case tar.TypeXGlobalHeader:
			// pax metadata for the archive, not an entry: nothing lands.
		default:
			odd = true
		}
		return nil
	}
	if err := c.walkBundle(bundle, guard); err != nil {
		return err
	}
	if odd {
		c.errorf("handover: refusing %s: an archive entry is not a regular file or a directory (a link, a device or a FIFO)", bundle)
		return ErrHandled
	}
	extract := func(tr *tar.Reader, hdr *tar.Header) error {
		target := filepath.Join(dir, filepath.Clean("/"+hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			return os.MkdirAll(target, 0o700)
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			// 0600 whatever mode the archive gives: the files are keys.
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return c.extractCopyError(bundle, target, err)
			}
			return out.Close()
		}
		return nil
	}
	return c.walkBundle(bundle, extract)
}

// extractCopyError classifies a failed io.Copy of one entry: the target
// file's own write error (a *fs.PathError with Op "write": disk full, a
// closed file system) names the write; anything else, including a read
// error on the bundle file itself (Op "read"), came from the gzip/tar
// side, so the archive is the unreadable one.
func (c *Context) extractCopyError(bundle, target string, err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok && pe.Op == "write" {
		c.errorf("handover: cannot write %s from %s: %v", target, bundle, err)
		return ErrHandled
	}
	return c.notArchive(bundle)
}

// walkBundle opens the .tar.gz and calls visit for every entry, in order.
// A file that is not a gzip tar is reported as notArchive.
func (c *Context) walkBundle(bundle string, visit func(*tar.Reader, *tar.Header) error) error {
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
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return c.notArchive(bundle)
		}
		if err := visit(tr, hdr); err != nil {
			return err
		}
	}
}

func (c *Context) notArchive(bundle string) error {
	c.errorf("handover: %s is neither a bundle directory nor a readable .tar.gz archive", bundle)
	return ErrHandled
}

// validateBundle ports handover::validate_bundle: every contract §2 key must
// exist and be non-empty, and every key that is there (an optional one too)
// must be a regular file with one name: not a link (Lstat), and not a hard
// link (the link count). The handover reads and sends each key from the
// bundle itself, never from a path that a link names or a second name of a
// file outside the bundle, for a .tar.gz and for a directory the user
// gives. The error NAMES the key.
func (c *Context) validateBundle(dir string) error {
	for _, key := range append(append([]string{}, handoverBundleKeys...), handoverOptionalKeys...) {
		info, err := c.lstat(filepath.Join(dir, key))
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			c.errorf("handover: bundle key %s is not a regular file (a link, a directory or a special file): %s/%s", key, dir, key)
			return ErrHandled
		}
		// A link count that cannot be read fails too (singleLink).
		if !singleLink(info) {
			c.errorf("handover: bundle key %s has more than one name (a hard link), so it can be a file outside the bundle: %s/%s", key, dir, key)
			return ErrHandled
		}
	}
	for _, key := range handoverBundleKeys {
		info, err := c.lstat(filepath.Join(dir, key))
		if err != nil || info.Size() == 0 {
			c.errorf("handover: bundle is incomplete — missing or empty key: %s (expected at %s/%s)", key, dir, key)
			return ErrHandled
		}
	}
	return nil
}

// placePKI ports handover::place_pki: seed the exported PKI into
// <k8sDir>/pki (kubeadm reuses EXISTING files instead of minting new ones).
// The pki dir is set to 0700 and checked before a key lands in it: kubeadm
// makes it 0755, and a dir of an earlier run can be open. Each file then
// goes in through installFile (certs and sa.pub 0644, keys 0600). A
// failure removes the files that this call placed, or names the ones it
// could not remove.
func (c *Context) placePKI(bundle, k8sDir string) error {
	pki := filepath.Join(k8sDir, "pki")
	if err := os.MkdirAll(pki, 0o700); err != nil {
		return err
	}
	if err := c.chmod(pki, 0o700); err != nil { // #nosec G302 -- a directory: 0700 is owner-only
		c.errorf("handover: cannot set %s to mode 700, so the keys would not be private in it", pki)
		return ErrHandled
	}
	if info, err := c.lstat(pki); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		c.errorf("handover: cannot set %s to mode 700, so the keys would not be private in it", pki)
		return ErrHandled
	}
	var placed []string
	for _, key := range handoverPKIKeys {
		// The PUBLIC halves (certs, sa.pub) are 0644; the keys are 0600.
		mode := fs.FileMode(0o644)
		if strings.HasSuffix(key, ".key") {
			mode = 0o600
		}
		data, err := os.ReadFile(filepath.Join(bundle, key))
		if err == nil {
			err = c.installFile(filepath.Join(pki, key), data, mode)
		}
		if err != nil {
			c.errorf("handover: could not place %s into %s with mode %s", key, pki, strconv.FormatUint(uint64(mode), 8))
			var left []string
			for _, k := range placed {
				if rerr := os.Remove(filepath.Join(pki, k)); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
					left = append(left, filepath.Join(pki, k))
				}
			}
			if len(left) > 0 {
				c.errorf("handover: could not remove the files that this step placed: %s. Delete them yourself.", strings.Join(left, " "))
			}
			return ErrHandled
		}
		placed = append(placed, key)
	}
	return nil
}

// installFile ports handover::install_file: write data to dst with mode,
// through a new file in the dir of dst. os.CreateTemp makes it 0600, chmod
// sets the mode, and the rename replaces dst. Thus the bytes never land in
// a file with a wider mode, and an existing dst (a run with --force) is
// replaced: its own mode, a link there or a second name of it does not get
// the bytes. A dir at dst (or a link to one) fails, as in the bash twin. On
// a failure the new file goes, and dst is as it was. A new file that cannot
// go is named: it can hold key bytes.
func (c *Context) installFile(dst string, data []byte, mode fs.FileMode) error {
	if info, err := os.Stat(dst); err == nil && info.IsDir() {
		return errors.New(dst + " is a directory")
	}
	f, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = c.chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			c.errorf("handover: could not remove %s, which can hold key bytes. Delete it yourself.", tmp)
		}
	}
	return err
}

// stripQuery drops everything from '?' on — for URLs that carry a credential
// (presigned object-store links) and must not be echoed whole.
func stripQuery(u string) string {
	if before, _, ok := strings.Cut(u, "?"); ok {
		return before
	}
	return u
}

// fetchSnapshot ports handover::fetch_snapshot: an explicit --snapshot wins;
// otherwise the bundle's snapshot-location is fetched BEST-EFFORT into the
// private workdir (0600). Every failure names the fix.
func (c *Context) fetchSnapshot(ctx context.Context, bundle, override, workdir string) (string, error) {
	if override != "" {
		if !fsutil.FileExists(override) {
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
		if err := c.makePrivate(d); err != nil {
			c.removeDir(d)
			return "", err
		}
		workdir = d
	}
	out := filepath.Join(workdir, "kubehz-handover-snapshot.db")
	raw, err := os.ReadFile(filepath.Join(bundle, "snapshot-location"))
	if err != nil {
		return "", err
	}
	location := execx.TrimNewlines(string(raw))
	switch {
	case strings.HasPrefix(location, "kkp://"):
		c.errorf("handover: snapshot-location '%s' is a KKP-internal locator (kkp://<destination>/<clusterID>/<backupName>) — it cannot be fetched from this node. Obtain the snapshot object via the platform (the tier2/api bundle download provides it) and re-run with --snapshot <file>", location)
		return "", ErrHandled
	case strings.HasPrefix(location, "s3://"):
		if !c.lookPath("aws") {
			c.errorf("handover: the snapshot lives at %s but no aws CLI is on this node — download it with any s3 client against the platform's object store and re-run with --snapshot <file>", location)
			return "", ErrHandled
		}
		if err := c.touch600(out); err != nil {
			return "", err
		}
		if _, err := c.capture(ctx, false, "aws", "s3", "cp", location, out); err != nil {
			c.errorf("handover: fetching %s via the aws CLI failed — download it yourself and re-run with --snapshot <file>", location)
			return "", ErrHandled
		}
		if err := c.chmod(out, 0o600); err != nil {
			return "", err
		}
	case strings.HasPrefix(location, "https://"):
		if err := c.touch600(out); err != nil {
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
		if !fsutil.FileExists(location) {
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
func (c *Context) touch600(path string) error {
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return err
	}
	return c.chmod(path, 0o600)
}

// memberIdentity ports handover::member_identity: the etcd member name +
// peer URL kubeadm's static pod will start with (lowercased hostname; the
// default-route source address).
func (c *Context) memberIdentity(ctx context.Context) (string, string, error) {
	host, err := c.hostname()
	if err != nil {
		host = ""
	}
	nodeName := strings.ToLower(execx.TrimNewlines(host))
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
	for line := range strings.SplitSeq(out, "\n") {
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
	// The file holds the key: installFile writes it 0600 from the first
	// byte, also over a file of an earlier run (os.WriteFile keeps the mode
	// of a file that is there).
	if err := c.installFile(out, []byte(content), 0o600); err != nil {
		c.errorf("handover: could not write %s with mode 600", out)
		return "", ErrHandled
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
//
// The InitConfiguration sets one value: a TTL of 1 h (kubeadm's default is
// 24 h) for the bootstrap token that kubeadm init creates. No later step of
// the handover uses that token: verify and the untaint use admin.conf, and
// --skip-token-print keeps it off the screen. No step of the handover joins
// a node: a join after it needs a token of its own (for example from
// `kubeadm token create --print-join-command` on this node), and lok8s does
// not run that command. The cluster-info ConfigMap and the bootstrap RBAC
// that joins need do not depend on this token, and init still creates
// them. kubeadm fills in the default groups and usages. A TTL of 0 means
// "never expires", so the value must stay above 0.
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
		"      pathType: File\n" +
		"---\n" +
		"apiVersion: kubeadm.k8s.io/v1beta4\n" +
		"kind: InitConfiguration\n" +
		"bootstrapTokens:\n" +
		"  - ttl: 1h0m0s\n"
	return os.WriteFile(out, []byte(content), 0o644) // #nosec G306 -- a static-pod manifest; the secret it mounts is its own 0600 file
}

// kubeadmInit ports handover::kubeadm_init. --skip-token-print: the join
// line that kubeadm prints at the end holds its bootstrap token, and that
// output reaches the terminal (and a log). kubeadm prints the line with
// the token withheld instead. No verbosity flag: kubeadm at --v=8 or above
// can log the bodies of Secrets.
func (c *Context) kubeadmInit(ctx context.Context, config string) error {
	return c.run(ctx, "kubeadm", "init", "--config", config,
		"--ignore-preflight-errors=DirAvailable--var-lib-etcd",
		"--skip-phases=addon/coredns,addon/kube-proxy",
		"--skip-token-print")
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
		if err := c.sleep(ctx, 5*time.Second); err != nil {
			return err
		}
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

// receivePlan is what receivePreflight resolved before the node is
// touched.
type receivePlan struct {
	snapshotFile string
	nodeName     string
	peerURL      string
	etcdTag      string
	endpoint     string
}

// receivePreflight fails before touching the node: the target dirs must
// be empty (or --force), then the snapshot, the member identity, the etcd
// pin and EVERY bundle-derived value are resolved first.
func (c *Context) receivePreflight(ctx context.Context, o ReceiveOpts, bundleDir, workdir, k8sDir, etcdDir string) (receivePlan, error) {
	var pre receivePlan
	// "not empty" means REAL content: kubeadm's package skeleton dirs
	// (manifests/, pki/) never block.
	if info, err := os.Stat(k8sDir); err == nil && info.IsDir() {
		real, findErr := firstRealContent(k8sDir)
		if findErr != nil && real == "" && !o.Force {
			c.errorf("handover: cannot inspect %s (find exit %d) — refusing to assume it is empty; fix permissions or re-run with --force.", k8sDir, 1)
			return pre, ErrHandled
		}
		if real != "" && !o.Force {
			c.errorf("handover: %s holds existing content (%s) — either this node already runs (or ran) Kubernetes, or a previous receive attempt left its seeded state behind. Clean up first ('kubeadm reset' clears the seeded PKI and etcd data), then re-run — with --force if you really mean to overwrite what remains.", k8sDir, real)
			return pre, ErrHandled
		}
	}
	if entries, err := os.ReadDir(etcdDir); err == nil && len(entries) > 0 {
		if o.Force {
			_ = os.RemoveAll(etcdDir)
		} else {
			c.errorf("handover: %s is not empty — refusing to restore over existing etcd data (re-run with --force to overwrite).", etcdDir)
			return pre, ErrHandled
		}
	}

	var err error
	pre.snapshotFile, err = c.fetchSnapshot(ctx, bundleDir, o.Snapshot, workdir)
	if err != nil {
		return pre, err
	}
	pre.nodeName, pre.peerURL, err = c.memberIdentity(ctx)
	if err != nil {
		return pre, err
	}
	pre.etcdTag = c.getenv("KUBEHZ_HANDOVER_ETCD_IMAGE_TAG")
	bundleTag := bundleValue(bundleDir, "etcd-version", "")
	if pre.etcdTag == "" {
		pre.etcdTag = bundleTag
		if pre.etcdTag == "" {
			pre.etcdTag = "3.5.21-0"
		}
	} else if bundleTag != "" && bundleTag != pre.etcdTag {
		c.warnf("handover: KUBEHZ_HANDOVER_ETCD_IMAGE_TAG '%s' overrides the bundle's etcd-version '%s'", pre.etcdTag, bundleTag)
	}
	if !imageTagRe.MatchString(pre.etcdTag) {
		c.errorf("handover: etcd image tag '%s' is not a plain image tag — refusing before touching the node", head(pre.etcdTag, 64))
		return pre, ErrHandled
	}
	if _, _, err := c.encryptionMetadata(bundleDir); err != nil {
		return pre, err
	}
	if _, err := c.encryptionKey(bundleDir); err != nil {
		return pre, err
	}
	pre.endpoint, err = c.endpointDNS(bundleDir)
	if err != nil {
		return pre, err
	}
	return pre, nil
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
	defer c.removeDir(workdir)
	if err := c.makePrivate(workdir); err != nil {
		return err
	}

	bundleDir, err := c.resolveBundle(o.Bundle, workdir)
	if err != nil {
		return err
	}
	if err := c.validateBundle(bundleDir); err != nil {
		return err
	}
	pre, err := c.receivePreflight(ctx, o, bundleDir, workdir, k8sDir, etcdDir)
	if err != nil {
		return err
	}
	endpoint := pre.endpoint

	if err := c.placePKI(bundleDir, k8sDir); err != nil {
		return err
	}
	if err := c.restoreSnapshot(ctx, pre.snapshotFile, etcdDir, pre.nodeName, pre.peerURL); err != nil {
		return err
	}
	if _, err := c.writeEncryptionConfig(bundleDir, k8sDir); err != nil {
		return err
	}
	kubeadmConfig := filepath.Join(k8sDir, "kubehz-handover-kubeadm.yaml")
	if err := c.writeKubeadmConfig(bundleDir, k8sDir, kubeadmConfig, pre.etcdTag); err != nil {
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
	// ssh reads the value of `-o UserKnownHostsFile=<path>` as a list of
	// files separated by whitespace. A path with a space becomes two
	// known_hosts files, and neither is the file the caller named.
	if strings.ContainsAny(o.KnownHosts, " \t\r\n") {
		c.errorf("handover: --known-hosts path must not contain whitespace: %q", o.KnownHosts)
		return ErrHandled
	}
	// A missing path, /dev/null, a dir, a FIFO or a device holds no key to
	// check against, and ssh cannot record a key in it.
	if o.KnownHosts != "" && !readableRegularFile(o.KnownHosts) {
		c.errorf("handover: --known-hosts must name an existing regular file that you can read: %q", o.KnownHosts)
		return ErrHandled
	}
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
	defer c.removeDir(workdir)
	if err := c.makePrivate(workdir); err != nil {
		return err
	}

	bundleDir, err := c.resolveBundle(o.Bundle, workdir)
	if err != nil {
		return err
	}
	if err := c.validateBundle(bundleDir); err != nil {
		return err
	}

	// These calls carry the CA private keys, so each one checks the host
	// key (StrictHostKeyChecking=yes): an unknown or a changed key fails
	// before a key leaves this machine. With --accept-new-host-key, only
	// the first call (the mkdir, which sends no key) takes an unknown key
	// and records it (accept-new). The handover then checks that the key is
	// in the known_hosts file, and every later call uses yes. Never
	// StrictHostKeyChecking=no or UserKnownHostsFile=/dev/null here. The bash
	// twin (handover::preseed) uses the same options.
	// With --known-hosts, GlobalKnownHostsFile=/dev/null makes that file the
	// only known_hosts file: a key in the system files (/etc/ssh/
	// ssh_known_hosts) is then no trust source.
	// ControlMaster=no and ControlPath=none: a master connection that the
	// user's ssh config opens or shares (ControlMaster, ControlPath) would
	// carry these calls without a new host key check. ssh takes the first
	// value it gets for an option, so these win over the config.
	// NoHostAuthenticationForLocalhost=no: with yes, ssh skips the host key
	// check for a loopback address (a node behind a forwarded port), also
	// with StrictHostKeyChecking=yes. VerifyHostKeyDNS=no: with yes, ssh
	// takes a key from a DNS SSHFP record that is in no known_hosts file.
	// UpdateHostKeys=no: the node cannot add or remove keys in the
	// known_hosts file.
	sshOpts := []string{"-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-o", "LogLevel=ERROR", "-o", "ConnectTimeout=10",
		"-o", "ControlMaster=no", "-o", "ControlPath=none",
		"-o", "NoHostAuthenticationForLocalhost=no", "-o", "VerifyHostKeyDNS=no", "-o", "UpdateHostKeys=no"}
	if o.KnownHosts != "" {
		sshOpts = append(sshOpts, "-o", "UserKnownHostsFile="+o.KnownHosts, "-o", "GlobalKnownHostsFile=/dev/null")
	}
	if o.SSHKey != "" {
		sshOpts = append(sshOpts, "-i", o.SSHKey)
	}
	portStr := strconv.Itoa(port)
	target := user + "@" + o.Node
	firstContact := "yes"
	if o.AcceptNewHostKey {
		firstContact = "accept-new"
		c.warnf("handover: --accept-new-host-key: the first contact trusts the host key that %s shows, without a check", o.Node)
	}
	checked := func(policy string) []string {
		return append([]string{"-o", "StrictHostKeyChecking=" + policy}, sshOpts...)
	}
	ssh := func(policy, remote string) error {
		return c.run(ctx, "ssh", append(checked(policy), "-p", portStr, target, remote)...)
	}
	// The pki dir is 0700 before any key lands in it: umask 077 makes a new
	// dir 0700, and chmod sets a dir that is there already (kubeadm makes it
	// 0755). A chmod that fails fails the call.
	if err := ssh(firstContact, "umask 077 && mkdir -p /etc/kubernetes/pki && chmod 0700 /etc/kubernetes/pki"); err != nil {
		c.errorf("handover: cannot reach %s over SSH, or cannot set /etc/kubernetes/pki on it to mode 700", target)
		if o.AcceptNewHostKey {
			c.errorf("handover: a changed host key also fails here; check the node and the known_hosts entry before you retry")
		} else {
			c.errorf("handover: an unknown or changed host key also fails here. Compare the fingerprints of \"ssh-keyscan -p %s %s | ssh-keygen -lf -\" with \"ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub\" on the node's console, and add the key to the known_hosts file if they match. Or retry with --accept-new-host-key to trust the first contact.", portStr, o.Node)
		}
		return ErrHandled
	}
	if o.AcceptNewHostKey {
		knownHosts := o.KnownHosts
		if knownHosts == "" {
			knownHosts = c.getenv("HOME") + "/.ssh/known_hosts"
		}
		if err := c.hostKeyRecorded(ctx, o.Node, port, knownHosts); err != nil {
			return err
		}
	}
	// bash command substitution drops each NUL byte of the output, so the
	// twins read the same lines.
	remote := func(cmd string) (string, error) {
		out, err := c.capture(ctx, false, "ssh", append(checked("yes"), "-p", portStr, target, cmd)...)
		return strings.ReplaceAll(out, "\x00", ""), err
	}

	// scp writes into a file that is there already: a hard link at the
	// destination gives the key bytes to its other name too, and a symlink
	// sends them to its target. So each file goes to a new staging file in
	// the 0700 pki dir first. One call checks the dirs and makes the files
	// (umask 077 and mktemp: mode 0600) and prints their names, the scp
	// calls fill them, and one call sets each mode and renames each file
	// over its destination. A rename (mv -T) replaces a link or a file there
	// and never writes through it. A failure removes the staging files, or
	// names them when that fails too. The bash twin does the same steps.
	out, err := remote(preseedStageCmd())
	staged, failed := preseedStageResult(out, err == nil, o.Node)
	if failed == "" {
		for i, key := range handoverPKIKeys {
			args := append(checked("yes"), "-P", portStr, filepath.Join(bundleDir, key), target+":"+staged[i])
			if err := c.run(ctx, "scp", args...); err != nil {
				failed = "copying " + key + " to " + o.Node + " failed"
				break
			}
		}
	}
	if failed == "" {
		out, err = remote(preseedInstallCmd(staged))
		if err != nil {
			failed = preseedInstallFailure(lastLine(out), o.Node)
		}
	}
	if failed != "" {
		c.errorf("handover: %s", failed)
		if len(staged) > 0 {
			if err := ssh("yes", "rm -f -- "+strings.Join(staged, " ")); err != nil {
				c.errorf("handover: could not remove the staging files on %s; they can hold key bytes. Remove them yourself: %s", o.Node, strings.Join(staged, " "))
			}
		}
		return ErrHandled
	}
	c.echo("handover: PKI pre-seeded on %s. Provision now (lo provision) — kubeone's kubeadm reuses the existing CA files, preserving cluster identity.", o.Node)
	return nil
}

// preseedPKIDir is the pki dir on the node that preseed writes into.
const preseedPKIDir = "/etc/kubernetes/pki"

// stagingName ports handover::staging_name: it reports whether name is the
// shape of a staging file that preseed's mktemp makes for key:
// /etc/kubernetes/pki/.<key>. and 10 ASCII letters or digits.
func stagingName(key, name string) bool {
	rest, ok := strings.CutPrefix(name, preseedPKIDir+"/."+key+".")
	if !ok || len(rest) != 10 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		b := rest[i]
		if (b < 'A' || b > 'Z') && (b < 'a' || b > 'z') && (b < '0' || b > '9') {
			return false
		}
	}
	return true
}

// preseedTrustedDirs are the dirs on the node that the stage call checks
// before it makes a staging file. The owner of such a dir (or a user who
// can write it) could put a link at a staging name between the mktemp and
// the scp, and scp would write the key through it. So each one must be a
// dir, not a link, that root or the ssh user owns, with no write bit for
// the group or others. The dirs above them (/etc) are not checked.
var preseedTrustedDirs = []string{"/etc/kubernetes", preseedPKIDir}

// preseedStageCmd ports handover::stage_cmd: the remote command of the
// stage call. Before each check it prints "check <dir> <uid> <mode>
// <type>" (GNU stat), and after the checks "stage", so the last line of a
// failed call names the dir that failed. Then it makes one staging file per
// PKI key (umask 077 and mktemp: mode 0600) and prints its name.
func preseedStageCmd() string {
	steps := []string{"umask 077"}
	for _, dir := range preseedTrustedDirs {
		steps = append(steps,
			`echo "check `+dir+` $(stat -c '%u %a %F' -- `+dir+`)"`,
			`test -n "$(find `+dir+` -maxdepth 0 -type d ! -perm /022 \( -uid 0 -o -uid "$(id -u)" \) -print)"`)
	}
	steps = append(steps, "echo stage")
	for _, key := range handoverPKIKeys {
		steps = append(steps, "mktemp "+preseedPKIDir+"/."+key+".XXXXXXXXXX")
	}
	return strings.Join(steps, " && ")
}

// preseedStageResult ports the stage step of handover::preseed: it reads
// the output of the stage call (preseedStageCmd); ok is false when the call
// failed. With the expected lines, failed is empty and names holds the
// staging names, one per PKI key, in order. Else failed says what is wrong,
// and names holds each line that is a staging name of some key, so that the
// cleanup can remove it. A line of another shape never goes into a command.
func preseedStageResult(out string, ok bool, node string) (names []string, failed string) {
	var lines []string
	if s := strings.TrimRight(out, "\n"); s != "" {
		lines = strings.Split(s, "\n")
	}
	if ok {
		failed = preseedStageLines(lines, node)
	} else {
		failed = preseedStageFailure(lastLine(out), node)
	}
	if failed == "" {
		return lines[len(preseedTrustedDirs)+1:], ""
	}
	for _, line := range lines {
		for _, key := range handoverPKIKeys {
			if stagingName(key, line) {
				names = append(names, line)
				break
			}
		}
	}
	return names, failed
}

// preseedStageFailure ports handover::stage_failure: the cause of a failed
// stage call from the last line that it printed. A check line names the
// dir that failed its check and what stat showed.
func preseedStageFailure(last, node string) string {
	for _, dir := range preseedTrustedDirs {
		if facts, ok := strings.CutPrefix(last, "check "+dir+" "); ok {
			return dir + " on " + node + " must be a dir, not a link, that root or the ssh user owns, with no write bit for the group or others. stat shows (uid, mode, type) " +
				preseedQuote(facts) + ". No key was sent"
		}
	}
	return "could not make the staging files " + preseedPKIDir + "/.<key>.XXXXXXXXXX on " + node
}

// preseedStageLines ports handover::stage_lines: the problem with the
// output of a stage call that passed, or "". It must be a check line per
// trusted dir, "stage", then one staging name per PKI key, in order.
func preseedStageLines(lines []string, node string) string {
	at := func(i int) string {
		if i < len(lines) {
			return lines[i]
		}
		return ""
	}
	for i, dir := range preseedTrustedDirs {
		if !strings.HasPrefix(at(i), "check "+dir+" ") {
			return node + " printed " + preseedQuote(at(i)) + " where the stage call prints the check of " + dir
		}
	}
	head := len(preseedTrustedDirs)
	if at(head) != "stage" {
		return node + " printed " + preseedQuote(at(head)) + ` where the stage call prints "stage"`
	}
	for i, key := range handoverPKIKeys {
		if !stagingName(key, at(head+1+i)) {
			return fmt.Sprintf("%s printed a staging name for %s that is not %s/.%s. and 10 letters or digits: %s",
				node, key, preseedPKIDir, key, preseedQuote(at(head+1+i)))
		}
	}
	if n := len(lines) - head - 1; n != len(handoverPKIKeys) {
		return fmt.Sprintf("%s printed %d lines for %d staging names", node, n, len(handoverPKIKeys))
	}
	return ""
}

// preseedQuoteLimit is the number of bytes of a line from the node that a
// message shows.
const preseedQuoteLimit = 200

// preseedQuote ports handover::quote: a line from the node, safe to print.
// It shows the first preseedQuoteLimit bytes in double quotes. A byte below
// 0x20, 0x7f and a byte from 0x80 shows as \xNN (so no terminal control,
// C1 controls too, reaches the terminal), a backslash as \\ and a double
// quote as \". A longer line also says how many bytes it has.
func preseedQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s) && i < preseedQuoteLimit; i++ {
		switch c := s[i]; {
		case c == '\\':
			b.WriteString(`\\`)
		case c == '"':
			b.WriteString(`\"`)
		case c >= 0x20 && c < 0x7f:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	b.WriteByte('"')
	if len(s) > preseedQuoteLimit {
		fmt.Fprintf(&b, " (the first %d of %d bytes)", preseedQuoteLimit, len(s))
	}
	return b.String()
}

// preseedInstallCmd ports handover::install_cmd: the remote command of the
// install call for the staging names (one per PKI key, in order). First it
// sets the mode of each staging file (the keys 0600, the certs and sa.pub
// 0644), then it renames each over its destination. Before each step it
// prints the step, so the last line of a failed call names the step that
// failed.
func preseedInstallCmd(staged []string) string {
	var steps []string
	for i, key := range handoverPKIKeys {
		mode := "0644"
		if handoverPKIRe.MatchString(key) {
			mode = "0600"
		}
		steps = append(steps, "echo 'chmod "+key+"'", "chmod "+mode+" "+staged[i])
	}
	for i, key := range handoverPKIKeys {
		steps = append(steps, "echo 'mv "+key+"'", "mv -f -T "+staged[i]+" "+preseedPKIDir+"/"+key)
	}
	return strings.Join(steps, " && ")
}

// preseedInstallFailure ports handover::install_failure: the cause of a
// failed install call from the last line that it printed. A mv failure
// names the files that went into place before it. A last line that names
// no step (a lost connection, or text from the node's shell) says that the
// state is unknown and names each destination to check.
func preseedInstallFailure(last, node string) string {
	var placed []string
	for _, key := range handoverPKIKeys {
		switch last {
		case "chmod " + key:
			return "could not set the mode of the staging file of " + key + " on " + node + "; no file was moved into place"
		case "mv " + key:
			before := "none"
			if len(placed) > 0 {
				before = strings.Join(placed, " ")
			}
			return "could not move the staging file of " + key + " to " + preseedPKIDir + "/" + key + " on " + node + "; in place before it: " + before
		}
		placed = append(placed, key)
	}
	dests := make([]string, 0, len(handoverPKIKeys))
	for _, key := range handoverPKIKeys {
		dests = append(dests, preseedPKIDir+"/"+key)
	}
	return "the install call on " + node + " failed, and its output names no step, so the state of the install is UNKNOWN. Check each of these files on " +
		node + ": " + strings.Join(dests, " ")
}

// lastLine is bash "${out##*$'\n'}" after a command substitution: the
// last line of out without its trailing newlines.
func lastLine(out string) string {
	out = strings.TrimRight(out, "\n")
	return out[strings.LastIndexByte(out, '\n')+1:]
}

// readableRegularFile is bash `[[ -f path && -r path ]]`: it follows a
// link, so a link to /dev/null fails too.
func readableRegularFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(path) // #nosec G304 -- the caller's own --known-hosts path, opened only to see that it is readable
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// hostKeyRecorded ports handover::host_key_recorded: ssh must have recorded
// the node's host key in the known_hosts file that it uses (--known-hosts,
// else ssh's own user file), so the later calls check the key against that
// record. ssh records a node in lowercase, and on a port other than 22 as
// [node]:port. `ssh-keygen -F` also finds a hashed entry (HashKnownHosts).
func (c *Context) hostKeyRecorded(ctx context.Context, node string, port int, file string) error {
	host := strings.ToLower(node)
	if port != 22 {
		host = "[" + host + "]:" + strconv.Itoa(port)
	}
	if err := c.runQuiet(ctx, "ssh-keygen", "-F", host, "-f", file); err == nil {
		return nil
	}
	c.errorf("handover: the host key of %s is not in %s after the first contact, so the next calls could not check it. Make sure that ssh can write that file (or name the file with --known-hosts), then retry.", host, file)
	return ErrHandled
}
