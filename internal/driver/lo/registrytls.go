package lo

// registrytls.go — the registry TLS certificate. The Secret generator
// mints one leaf for the whole registry set; it lives in the set's docker
// volume `<project_network>-registry-tls` and every registry container
// mounts that volume at RegistryTLSMount. No project directory holds the
// material: the bash tree minted into $PATH_SECRETS (the flat store its
// entrypoint defaulted) and bind-mounted `<Base>/.secrets/tls/registries`
// (go-migration.md, D34).
//
// The volume is written and read through a throwaway container that
// mounts it (`docker container create` + `docker cp` + `docker rm`): the
// set's own containers may not exist yet (the first `lo up`, or after
// `lo registry down`), and one path for both directions keeps the code
// small. The private key never leaves the volume: `lo up` reads tls.crt
// (the config hash) and .sans (the re-mint key); nothing in the cluster
// consumes the key, the kind nodes trust the dev CA through certs.d.

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/toolchain"
	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

const (
	// registryTLSVolumeSuffix names the set's cert volume next to its data
	// volumes (`<project_network>-registry-<name>`).
	registryTLSVolumeSuffix = "-registry-tls"
	// registryTLSScratchPrefix is the scratch dir the mint hands the
	// generator as PATH_SECRETS, under the DOMAIN dir: never $TMPDIR (a
	// rename across filesystems fails, v0.3.1) and never a project store.
	registryTLSScratchPrefix = ".registry-tls-tmp."
	// legacyRegistryTLSRel is where every release before v0.4.0 (and the
	// bash tree) extracted the cert, relative to the project root.
	legacyRegistryTLSRel = ".secrets/tls/registries"
	// registryTLSSecretName and registryTLSSecretNS name the Secret the
	// mint asks the generator for (file-local: the cache key only).
	// #nosec G101 -- the generator's cache key (a Secret NAME), not a credential.
	registryTLSSecretName = "registries-tls"
	registryTLSSecretNS   = "lok8s-system"
)

// registryTLSFiles are the volume's entries: the pair the registries
// serve, and the SAN set they were minted for (the re-mint key).
var registryTLSFiles = []string{"tls.crt", "tls.key", ".sans"}

// tlsVolume is the set's cert volume.
func (f *RegistryFile) tlsVolume() string { return f.ProjectNetwork + registryTLSVolumeSuffix }

// registryTLSSANs builds the SAN list (hostnames first per entry, then its
// IP; deduplicated, order kept). Framework registries contribute their
// canonical hostname; mirrors contribute the upstream domain they
// impersonate. Every registry contributes its IP.
func registryTLSSANs(rf *RegistryFile) []string {
	var sans []string
	for _, r := range rf.Registries {
		if r.Host != "" {
			sans = append(sans, r.Host)
		}
		if r.Domain != "" {
			sans = append(sans, r.Domain)
		}
		if r.IP != "" {
			sans = append(sans, r.IP)
		}
	}
	seen := map[string]bool{}
	var uniq []string
	for _, s := range sans {
		if seen[s] {
			continue
		}
		seen[s] = true
		uniq = append(uniq, s)
	}
	return uniq
}

// mintEnv is the in-process generator's env lookup: PATH_SECRETS answers
// with the scratch store the mint chose, every other key comes from the
// process environment (plugin.DefaultEnv), exactly what the exec'd child
// sees with the one entry appended to its inherited env.
func mintEnv(pathSecrets string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if key == secret.PathSecretsEnv {
			return pathSecrets, true
		}
		return plugin.DefaultEnv(key)
	}
}

// registriesTLSCert makes sure the set's volume holds a cert for the
// current SAN set (bash: lo::registries_tls_cert). No-op unless
// spec.registries.tls. Order: a missing volume is imported from the legacy
// directory when that holds a pair (one [warn], the files stay); an
// existing volume whose .sans equals the current SAN set is up to date;
// otherwise the generator mints a fresh leaf into the volume.
func (d *Driver) registriesTLSCert(ctx context.Context, domain string, errOut io.Writer) error {
	rf, err := regFile()
	if err != nil {
		// bash: registry::each prints the raw "error: …" line, then returns 1.
		fmt.Fprintln(errOut, err)
		return ui.Handled(err)
	}
	if !rf.TLS {
		return nil
	}
	sans := registryTLSSANs(rf)
	if len(sans) == 0 {
		fmt.Fprintln(errOut, "error: no registry SANs resolved — cannot mint registry TLS cert")
		return ui.Handled(fmt.Errorf("no registry SANs"))
	}
	vol := rf.tlsVolume()

	exists := d.volumeExists(ctx, vol)
	if !exists {
		imported, err := d.registryTLSImport(ctx, vol, errOut)
		if err != nil {
			return err
		}
		exists = imported
	}
	if exists {
		crt, prev, ok, err := d.registryTLSRead(ctx, vol, errOut)
		if err != nil {
			return err
		}
		if ok && strings.TrimRight(string(prev), "\n") == strings.Join(sans, "\n") {
			d.tlsCrt = crt
			ui.DebugTo(errOut, "registry TLS cert up to date (%d SANs)", len(sans))
			return nil
		}
	}
	_, err = d.registryTLSMint(ctx, domain, vol, exists, sans, errOut)
	return err
}

// registryTLSImport creates the volume from the legacy directory
// `<Base>/.secrets/tls/registries` when that holds a pair (a project that
// ran a release before v0.4.0 or the bash tree), byte for byte. The files
// stay: the running containers still bind-mount them until the operator
// recreates the set. Returns false when there is nothing to import.
func (d *Driver) registryTLSImport(ctx context.Context, vol string, errOut io.Writer) (bool, error) {
	legacy := filepath.Join(d.deps.Paths.Base, filepath.FromSlash(legacyRegistryTLSRel))
	hasCrt := fsutil.FileExists(filepath.Join(legacy, "tls.crt"))
	hasKey := fsutil.FileExists(filepath.Join(legacy, "tls.key"))
	if !hasCrt && !hasKey {
		return false, nil
	}
	if hasCrt != hasKey {
		// Half a pair is no pair: say so once, then mint fresh.
		ui.WarnTo(errOut, "registry TLS: the legacy directory %s holds an incomplete pair (tls.crt or tls.key is missing). Minting a fresh certificate.", legacy)
		return false, nil
	}
	// -L: a symlinked legacy file copies its target, not the link.
	if err := d.registryTLSStore(ctx, vol, false, legacy, true, errOut); err != nil {
		return false, err
	}
	ui.WarnTo(errOut, "registry TLS cert imported from %s into volume %s. The running registry containers still mount %s. Next: lo registry down && lo registry up", legacy, vol, legacy)
	return true, nil
}

// registryTLSMint drives the Secret generator for the SAN set and stores
// the result in the volume (created when exists is false). The generator
// gets a scratch PATH_SECRETS under the domain dir (its leaf cache and the
// extracted files), removed on every exit. Returns the cert PEM.
//
// The generator is the ONE cert: implementation (leaf cache, CA handling)
// — kustomize/plugins/secret, imported as a package (WP3): the manifest
// below is handed to secret.Run in-process exactly as it used to go to the
// plugin binary's stdin, and the Secret it emits is parsed the same way.
// Under LO_RENDER=exec the binary at KUSTOMIZE_PLUGIN_HOME is exec'd as
// before (built on demand through the KustomizeBuild hook). Do not inline a
// second cert mint path.
func (d *Driver) registryTLSMint(ctx context.Context, domain, vol string, exists bool, sans []string, errOut io.Writer) ([]byte, error) {
	// The mode is validated (an unknown LO_RENDER fails closed here as it
	// does in the render); which path mints is SecretInProcess: the
	// imported generator on both builds unless LO_RENDER=exec asks for the
	// plugin binary explicitly.
	if _, err := render.CurrentMode(); err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return nil, ui.Handled(err)
	}
	execPlugin := !render.SecretInProcess()
	var pluginBin string
	if execPlugin {
		pluginHome := config.KustomizePluginHome(d.deps.Paths)
		pluginBin = filepath.Join(pluginHome, filepath.FromSlash(toolchain.SecretPluginRel))
		// The Secret plugin mints the cert. It's needed across the lok8s
		// flow anyway, so build it on demand if it's missing and we can
		// (bash probed `declare -F kustomize::build`; the Go seam is the
		// injectable hook); otherwise fail with guidance.
		if !fsutil.IsExecutable(pluginBin) && d.Hooks.KustomizeBuild != nil {
			ui.DebugTo(errOut, "registry TLS: Secret plugin missing — building it (lo kustomize build)")
			_ = d.Hooks.KustomizeBuild(ctx)
		}
		if !fsutil.IsExecutable(pluginBin) {
			fmt.Fprintln(errOut, "error: spec.registries.tls is true (default) but the Secret plugin is not built at")
			fmt.Fprintf(errOut, "       %s. Build it with 'lo kustomize build' (needs go), or set\n", pluginBin)
			fmt.Fprintln(errOut, "       spec.registries.tls: false for plain-HTTP registries. Then retry.")
			return nil, ui.Handled(fmt.Errorf("secret plugin not built at %s", pluginBin))
		}
	}

	domainDir := filepath.Join(d.deps.Paths.Clusters, domain)
	if err := os.MkdirAll(domainDir, 0o755); err != nil {
		return nil, err
	}
	// A mint killed mid-way leaves its scratch (and the key in it) behind;
	// sweep every stale one before creating this run's.
	if matches, _ := filepath.Glob(filepath.Join(domainDir, registryTLSScratchPrefix+"*")); len(matches) > 0 {
		removed := 0
		for _, s := range matches {
			if info, err := os.Stat(s); err != nil || !info.IsDir() {
				continue // directories only, as the bash sweep
			}
			_ = os.RemoveAll(s)
			removed++
		}
		if removed > 0 {
			ui.DebugTo(errOut, "registry TLS: removed %d stale scratch dir(s) under %s", removed, domainDir)
		}
	}
	scratch, err := os.MkdirTemp(domainDir, registryTLSScratchPrefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	// JSON array of SANs (compact, jq -c shape); YAML accepts it inline.
	var hostsJSON strings.Builder
	hostsJSON.WriteByte('[')
	for i, s := range sans {
		if i > 0 {
			hostsJSON.WriteByte(',')
		}
		fmt.Fprintf(&hostsJSON, "%q", s)
	}
	hostsJSON.WriteByte(']')

	manifest := fmt.Sprintf(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: kubernetes.io/tls
cert:
  hosts: %s
`, registryTLSSecretName, registryTLSSecretNS, hostsJSON.String())

	var out strings.Builder
	if execPlugin {
		// The scratch store rides on the child's env (appended after the
		// inherited environment, so it wins over an inherited
		// PATH_SECRETS); CAROOT and the rest are inherited as before.
		err = d.deps.Runner.Run(ctx, execx.Cmd{
			Name:   pluginBin, // absolute path — used as-is by the runner
			Env:    []string{secret.PathSecretsEnv + "=" + scratch},
			Stdin:  strings.NewReader(manifest),
			Stdout: &out,
			Stderr: d.stderr(),
		})
	} else {
		// In-process: no argv config path, so the generator reads the
		// manifest from stdin — the same protocol the exec above uses. A
		// failure is reported the way the plugin binary's main did
		// (plugin.Fail) on the same stream.
		err = secret.Run([]string{"Secret"}, strings.NewReader(manifest), &out, mintEnv(scratch))
		if err != nil {
			fmt.Fprintln(d.stderr(), "secret plugin:", err)
		}
	}
	if err != nil {
		fmt.Fprintln(errOut, "error: the Secret plugin failed to mint the registry TLS cert")
		return nil, ui.Handled(fmt.Errorf("secret plugin failed: %w", err))
	}

	var secretOut struct {
		Data map[string]string `yaml:"data"`
	}
	_ = yaml.Unmarshal([]byte(out.String()), &secretOut)
	crtRaw, crtErr := base64.StdEncoding.DecodeString(secretOut.Data["tls.crt"])
	keyRaw, keyErr := base64.StdEncoding.DecodeString(secretOut.Data["tls.key"])
	if crtErr != nil || keyErr != nil || len(crtRaw) == 0 || len(keyRaw) == 0 {
		fmt.Fprintln(errOut, "error: registry TLS cert extraction failed (plugin output had no tls.crt/tls.key)")
		return nil, ui.Handled(fmt.Errorf("secret plugin output missing tls.crt/tls.key"))
	}
	if err := os.WriteFile(filepath.Join(scratch, "tls.crt"), crtRaw, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(scratch, "tls.key"), keyRaw, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(scratch, ".sans"), []byte(strings.Join(sans, "\n")+"\n"), 0o644); err != nil {
		return nil, err
	}
	if err := d.registryTLSStore(ctx, vol, exists, scratch, false, errOut); err != nil {
		return nil, err
	}
	d.tlsCrt = crtRaw
	ui.DebugTo(errOut, "minted registry TLS cert with SANs: %s", strings.Join(sans, " "))
	return crtRaw, nil
}

// registryTLSStore copies the registryTLSFiles present in dir into the
// volume, creating it first when exists is false. Populated through a
// throwaway container that mounts the volume: no image of our own, no
// running registry needed. dereference adds `docker cp -L` (the import:
// a symlinked file copies its target).
func (d *Driver) registryTLSStore(ctx context.Context, vol string, exists bool, dir string, dereference bool, errOut io.Writer) error {
	if !exists {
		if errText, err := d.errOutput(ctx, "docker", "volume", "create", vol); err != nil {
			fmt.Fprintf(errOut, "error: docker volume create %s failed: %s\n", vol, firstLine(errText))
			return ui.Handled(fmt.Errorf("docker volume create %s: %w", vol, err))
		}
	}
	return d.withTLSVolume(ctx, vol, errOut, func(ctr string) error {
		for _, name := range registryTLSFiles {
			src := filepath.Join(dir, name)
			if !fsutil.FileExists(src) {
				continue
			}
			args := []string{"cp"}
			if dereference {
				args = append(args, "-L")
			}
			args = append(args, src, ctr+":"+RegistryTLSMount+"/"+name)
			if errText, err := d.errOutput(ctx, "docker", args...); err != nil {
				fmt.Fprintf(errOut, "error: docker cp %s into volume %s failed: %s\n", name, vol, firstLine(errText))
				return ui.Handled(fmt.Errorf("docker cp %s into %s: %w", name, vol, err))
			}
		}
		return nil
	})
}

// registryTLSRead returns tls.crt and .sans from the volume. ok is false
// when the volume holds no complete pair: tls.crt must decode to a PEM
// CERTIFICATE block and tls.key to a PEM block (its bytes are checked and
// discarded, never kept), so an empty, truncated or non-PEM file (a crash
// mid `docker cp`) counts as missing and the caller mints again. err is a
// docker failure (the throwaway container could not be created); the
// error line is already on errOut.
func (d *Driver) registryTLSRead(ctx context.Context, vol string, errOut io.Writer) (crt, sans []byte, ok bool, err error) {
	err = d.withTLSVolume(ctx, vol, errOut, func(ctr string) error {
		var hasCrt bool
		crt, hasCrt = d.volumeRead(ctx, ctr, "tls.crt")
		key, hasKey := d.volumeRead(ctx, ctr, "tls.key")
		sans, _ = d.volumeRead(ctx, ctr, ".sans")
		ok = hasCrt && hasKey && pemBlockIs(crt, "CERTIFICATE") && pemBlockIs(key, "")
		return nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	if !ok {
		crt = nil
	}
	return crt, sans, ok, nil
}

// pemBlockIs reports whether data starts with a PEM block, of the given
// type when typ is not empty.
func pemBlockIs(data []byte, typ string) bool {
	block, _ := pem.Decode(data)
	return block != nil && (typ == "" || block.Type == typ)
}

// withTLSVolume runs fn against a throwaway container that mounts vol at
// RegistryTLSMount (`docker cp` needs a container). The container is
// removed on every exit.
func (d *Driver) withTLSVolume(ctx context.Context, vol string, errOut io.Writer, fn func(ctr string) error) error {
	ctr := vol + "-io"
	_ = d.runQuiet(ctx, "docker", "rm", "-f", ctr)
	if errText, err := d.errOutput(ctx, "docker", "container", "create", "--name", ctr,
		"--volume", vol+":"+RegistryTLSMount, RegistryImage); err != nil {
		fmt.Fprintf(errOut, "error: docker container create %s (volume %s) failed: %s\n", ctr, vol, firstLine(errText))
		return ui.Handled(fmt.Errorf("docker container create %s: %w", ctr, err))
	}
	defer func() { _ = d.runQuiet(ctx, "docker", "rm", "-f", ctr) }()
	return fn(ctr)
}

// volumeRead returns one file of the mounted volume through
// `docker cp <ctr>:<mount>/<name> -`, a tar stream with that one entry.
// ok is false when docker fails (no such file) or the stream is empty.
func (d *Driver) volumeRead(ctx context.Context, ctr, name string) ([]byte, bool) {
	var out bytes.Buffer
	err := d.deps.Runner.Run(ctx, execx.Cmd{
		Name: "docker", Args: []string{"cp", ctr + ":" + RegistryTLSMount + "/" + name, "-"},
		Stdout: &out, Stderr: io.Discard,
	})
	if err != nil {
		return nil, false
	}
	return tarFirstFile(out.Bytes())
}

// tarFileLimit bounds one volume entry (a PEM pair is a few KiB); an
// entry above it is refused, never truncated.
const tarFileLimit = 1 << 20

// tarFirstFile returns the first regular entry of a tar stream. ok is
// false for an empty entry (a crash mid `docker cp` leaves one), an entry
// above tarFileLimit, or a body shorter than its header says.
func tarFirstFile(stream []byte) ([]byte, bool) {
	tr := tar.NewReader(bytes.NewReader(stream))
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, false
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size <= 0 || h.Size > tarFileLimit {
			return nil, false
		}
		data := make([]byte, h.Size)
		if _, err := io.ReadFull(tr, data); err != nil {
			return nil, false
		}
		return data, true
	}
}

// volumeExists is `docker volume inspect <name>`.
func (d *Driver) volumeExists(ctx context.Context, name string) bool {
	_, err := d.output(ctx, "docker", "volume", "inspect", "-f", "{{.Name}}", name)
	return err == nil
}

// firstLine trims a captured stderr to its first line.
func firstLine(s string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return first
}

// registryMount is what one registry container mounts at RegistryTLSMount.
type registryMount struct {
	Container string
	// Kind is "volume", "bind", "none" (a container without the mount:
	// plain mode) or "absent" (no such container).
	Kind string
	// Source is the volume name or the bind-mount source directory.
	Source string
}

// registryCertMounts inspects every registry container of the set.
func (d *Driver) registryCertMounts(ctx context.Context, rf *RegistryFile) []registryMount {
	var mounts []registryMount
	for _, r := range rf.Registries {
		name, _ := rf.containerFor(r.Name)
		m := registryMount{Container: name, Kind: "absent"}
		out, err := d.output(ctx, "docker", "inspect", "-f",
			`{{range .Mounts}}{{.Destination}}|{{.Type}}|{{.Name}}|{{.Source}}{{"\n"}}{{end}}`, name)
		if err == nil {
			m.Kind = "none"
			for line := range strings.SplitSeq(out, "\n") {
				f := strings.Split(line, "|")
				if len(f) != 4 || f[0] != RegistryTLSMount {
					continue
				}
				m.Kind = f[1]
				m.Source = f[3]
				if f[1] == "volume" {
					m.Source = f[2]
				}
			}
		}
		mounts = append(mounts, m)
	}
	return mounts
}

// RegistryTLSStatus prints the set's certificate and what each container
// mounts (`lo registry tls status`, Go-only).
func (d *Driver) RegistryTLSStatus(ctx context.Context, domain string, out, errOut io.Writer) error {
	if err := d.registryInit(domain, errOut); err != nil {
		return err
	}
	rf, err := regFile()
	if err != nil {
		fmt.Fprintln(errOut, err)
		return ui.Handled(err)
	}
	if !rf.TLS {
		fmt.Fprintf(out, "registry TLS  off (spec.registries.tls: false in %s)\n", domain)
		return nil
	}
	vol := rf.tlsVolume()
	fmt.Fprintf(out, "registry TLS  on\n")
	fmt.Fprintf(out, "volume        %s\n", vol)
	if !d.volumeExists(ctx, vol) {
		fmt.Fprintf(out, "certificate   none. The volume does not exist. Next: lo up\n")
	} else if crt, sans, ok, err := d.registryTLSRead(ctx, vol, errOut); err != nil {
		return err
	} else if !ok {
		fmt.Fprintf(out, "certificate   none. The volume holds no complete tls.crt + tls.key pair. Next: lo registry tls renew\n")
	} else {
		leaf, err := parseLeaf(crt)
		if err != nil {
			fmt.Fprintf(out, "certificate   unreadable: %v. Next: lo registry tls renew\n", err)
		} else {
			fmt.Fprintf(out, "not before    %s\n", leaf.NotBefore.UTC().Format("2006-01-02T15:04:05Z"))
			fmt.Fprintf(out, "not after     %s\n", leaf.NotAfter.UTC().Format("2006-01-02T15:04:05Z"))
			var names []string
			names = append(names, leaf.DNSNames...)
			for _, ip := range leaf.IPAddresses {
				names = append(names, ip.String())
			}
			fmt.Fprintf(out, "sans          %s\n", strings.Join(names, " "))
		}
		want := strings.Join(registryTLSSANs(rf), "\n")
		if strings.TrimRight(string(sans), "\n") != want {
			fmt.Fprintf(out, "san set       stale. The registries changed since the mint. Next: lo registry tls renew\n")
		}
	}
	fmt.Fprintln(out, "containers")
	for _, m := range d.registryCertMounts(ctx, rf) {
		switch m.Kind {
		case "volume":
			fmt.Fprintf(out, "  %-28s volume %s\n", m.Container, m.Source)
		case "bind":
			fmt.Fprintf(out, "  %-28s bind %s (legacy). Next: lo registry down && lo registry up\n", m.Container, m.Source)
		case "none":
			fmt.Fprintf(out, "  %-28s no certificate mount\n", m.Container)
		default:
			fmt.Fprintf(out, "  %-28s absent\n", m.Container)
		}
	}
	return nil
}

// RegistryTLSRenew mints a fresh certificate into the volume and restarts
// the set's containers (`lo registry tls renew`, Go-only). The containers
// keep their config-hash label, which carries the old cert signature, so
// the next `lo up` recreates them with the new one; the cluster nodes need
// nothing, they trust the dev CA.
func (d *Driver) RegistryTLSRenew(ctx context.Context, domain string, out, errOut io.Writer) error {
	if err := d.registryInit(domain, errOut); err != nil {
		return err
	}
	rf, err := regFile()
	if err != nil {
		fmt.Fprintln(errOut, err)
		return ui.Handled(err)
	}
	if !rf.TLS {
		fmt.Fprintf(errOut, "error: spec.registries.tls is false in %s. There is no certificate to renew.\n", domain)
		return ui.Handled(fmt.Errorf("registry TLS off for %s", domain))
	}
	sans := registryTLSSANs(rf)
	if len(sans) == 0 {
		fmt.Fprintln(errOut, "error: no registry SANs resolved — cannot mint registry TLS cert")
		return ui.Handled(fmt.Errorf("no registry SANs"))
	}
	vol := rf.tlsVolume()
	if _, err := d.registryTLSMint(ctx, domain, vol, d.volumeExists(ctx, vol), sans, errOut); err != nil {
		return err
	}
	fmt.Fprintf(out, "registry TLS renewed: volume %s, %d SANs\n", vol, len(sans))
	for _, m := range d.registryCertMounts(ctx, rf) {
		if m.Kind == "absent" {
			continue
		}
		if err := d.runQuiet(ctx, "docker", "restart", m.Container); err != nil {
			fmt.Fprintf(errOut, "error: docker restart %s failed. Next: lo registry down && lo registry up\n", m.Container)
			return ui.Handled(fmt.Errorf("docker restart %s: %w", m.Container, err))
		}
		fmt.Fprintf(out, "restarted %s\n", m.Container)
	}
	fmt.Fprintln(out, "Next: run 'lo up' in each cluster that uses the set. It picks the certificate up there.")
	return nil
}

// RegistryTLSDoctor is the `lo doctor` line for the set's certificate
// mounts: "" when TLS is off, no set is configured or none of its
// containers exist (docker is not consulted for a domain without a
// registry JSON). warn is true for a legacy bind mount.
func (d *Driver) RegistryTLSDoctor(ctx context.Context, domain string) (line string, warn bool) {
	path := filepath.Join(d.deps.Paths.Clusters, domain, ".registries.json")
	if !fsutil.FileExists(path) {
		return "", false
	}
	rf, err := loadRegistryFile(path)
	if err != nil || !rf.TLS {
		return "", false
	}
	var volumes, binds, bindSrc []string
	for _, m := range d.registryCertMounts(ctx, rf) {
		switch m.Kind {
		case "volume":
			volumes = append(volumes, m.Container)
		case "bind":
			binds = append(binds, m.Container)
			bindSrc = append(bindSrc, m.Source)
		}
	}
	switch {
	case len(binds) > 0:
		return fmt.Sprintf("registry TLS: %d of %d containers mount the legacy directory %s. Next: lo registry down && lo registry up",
			len(binds), len(rf.Registries), bindSrc[0]), true
	case len(volumes) > 0:
		return fmt.Sprintf("registry TLS: %d containers mount volume %s", len(volumes), rf.tlsVolume()), false
	}
	return "", false
}

// parseLeaf parses the first certificate of a PEM bundle.
func parseLeaf(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}
