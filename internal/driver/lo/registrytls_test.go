package lo

// registrytls_test.go — the registry TLS certificate in the set's docker
// volume (registrytls.go): the mint through the Secret generator (exec
// stub and the real in-process generator), the volume as the only store,
// the legacy-directory import, the containers' volume mount, `lo registry
// tls status|renew` and the doctor line. Every docker call goes through
// the file-backed fake; no daemon, no project store, no /tmp.

import (
	"archive/tar"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/testutil"
)

const (
	tlsVol    = "lok8s-registry-tls"
	tlsVolIO  = "lok8s-registry-tls-io"
	tlsDomain = "test.lok8s.dev"
)

// tlsDriver is testDriver + the fake docker + the TLS spec (project
// network `lok8s`, one shared mirror) + the registry config templates, so
// both the mint and the container reconcile can run.
func tlsDriver(t *testing.T, tlsOn string) (*Driver, *fakeRunner, *fakeDocker, *bytes.Buffer, *config.Paths, string) {
	t.Helper()
	d, runner, errBuf, p := testDriver(t)
	fd := newFakeDocker(t)
	runner.handler = fd.handle
	cy := writeTLSSpec(t, p.Clusters, tlsOn)
	regDir := filepath.Join(p.Lok8s, "drivers", "lo", "cluster", "registry")
	testutil.WriteFile(t, filepath.Join(regDir, "build.yaml"), "version: 0.1\n")
	testutil.WriteFile(t, filepath.Join(regDir, "cache.yaml"), "version: 0.1\n")
	testutil.WriteFile(t, filepath.Join(regDir, "mirror.yaml"), realMirrorTemplate(t))
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatalf("readNetworkConfig: %v\n%s", err, errBuf.String())
	}
	return d, runner, fd, errBuf, p, cy
}

// pluginCall records what the exec'd Secret plugin stub saw and answers.
type pluginCall struct {
	Manifest string
	Env      []string
	// Crt/Key are what the stub emits (base64'd into the Secret).
	Crt, Key string
}

// stubSecretPlugin creates the plugin binary path on disk (executable —
// the mint stats it) and wires the runner to answer its exec: capture the
// manifest and the child env, emit a k8s Secret with the configured pair.
// This is the LO_RENDER=exec pipeline; the default in-process mint (the
// generator imported as a package) is covered by
// TestRegistriesTLSCertMintsInProcess. The previous handler keeps
// answering everything else (the fake docker).
func stubSecretPlugin(t *testing.T, runner *fakeRunner, base string) (pluginBin string, rec *pluginCall) {
	t.Helper()
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	pluginHome := filepath.Join(base, ".kustomize")
	pluginBin = filepath.Join(pluginHome, "secrets.lok8s.dev", "v1", "secret", "Secret")
	testutil.WriteFile(t, pluginBin, "#!/bin/sh\nexit 1\n") // never actually executed
	os.Chmod(pluginBin, 0o755)
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", pluginHome)

	rec = &pluginCall{Crt: "FAKECRT", Key: "FAKEKEY"}
	inner := runner.handler
	runner.handler = func(c execx.Cmd) error {
		if c.Name != pluginBin {
			if inner != nil {
				return inner(c)
			}
			return nil
		}
		var buf bytes.Buffer
		if c.Stdin != nil {
			buf.ReadFrom(c.Stdin)
		}
		rec.Manifest = buf.String()
		rec.Env = append([]string(nil), c.Env...)
		writeOut(c, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: registries-tls\n  namespace: lok8s-system\ntype: kubernetes.io/tls\ndata:\n  tls.crt: "+
			base64.StdEncoding.EncodeToString([]byte(rec.Crt))+"\n  tls.key: "+
			base64.StdEncoding.EncodeToString([]byte(rec.Key))+"\n")
		return nil
	}
	return pluginBin, rec
}

// scratchDirs lists the mint's scratch dirs left under the domain dir.
func scratchDirs(t *testing.T, p *config.Paths) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(p.Clusters, tlsDomain))
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), registryTLSScratchPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

func TestRegistriesTLSCertMintsIntoTheVolume(t *testing.T) {
	d, runner, fd, errBuf, p, _ := tlsDriver(t, "true")
	_, rec := stubSecretPlugin(t, runner, p.Base)

	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatalf("registriesTLSCert: %v\n%s", err, errBuf.String())
	}

	// The generator got a scratch PATH_SECRETS under the DOMAIN dir from
	// the mint (the process env carries none: testDriver scrubs it), and
	// the scratch is gone afterwards.
	got, ok := envValue(rec.Env, "PATH_SECRETS")
	if !ok {
		t.Fatalf("plugin child env %q carries no PATH_SECRETS", rec.Env)
	}
	wantPrefix := filepath.Join(p.Clusters, tlsDomain, registryTLSScratchPrefix)
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("PATH_SECRETS = %q, want a scratch under %q", got, wantPrefix)
	}
	if left := scratchDirs(t, p); len(left) != 0 {
		t.Fatalf("scratch dirs left behind: %v", left)
	}

	// SANs handed to the plugin as cert.hosts: framework hostnames, mirror
	// domain, IPs.
	for _, want := range []string{"lok8s.local", "lok8s.cache", "docker.io", "10.125.50.101", "10.125.50.102"} {
		if !strings.Contains(rec.Manifest, want) {
			t.Errorf("SAN %q missing from plugin manifest:\n%s", want, rec.Manifest)
		}
	}

	// The volume is the only store: the extracted pair and the SAN set
	// landed there, nothing under <Base>/.secrets.
	for file, want := range map[string]string{"tls.crt": "FAKECRT", "tls.key": "FAKEKEY"} {
		if got, ok := fd.volumeFile(tlsVol, file); !ok || got != want {
			t.Fatalf("volume %s/%s = %q, %v; want %q", tlsVol, file, got, ok, want)
		}
	}
	if sans, ok := fd.volumeFile(tlsVol, ".sans"); !ok || !strings.Contains(sans, "lok8s.local\n") {
		t.Fatalf("volume .sans = %q, %v", sans, ok)
	}
	if fsutil.DirExists(filepath.Join(p.Base, ".secrets")) {
		t.Fatal("the mint wrote into <Base>/.secrets")
	}

	// The docker sequence: inspect (absent), create the volume, populate it
	// through the throwaway container, remove that container.
	wantSeq := []string{
		"docker volume inspect -f {{.Name}} " + tlsVol,
		"docker volume create " + tlsVol,
		"docker rm -f " + tlsVolIO, // a leftover io container never shadows the create
		"docker container create --name " + tlsVolIO + " --volume " + tlsVol + ":" + RegistryTLSMount + " " + RegistryImage,
		"docker cp " + got + "/tls.crt " + tlsVolIO + ":" + RegistryTLSMount + "/tls.crt",
		"docker cp " + got + "/tls.key " + tlsVolIO + ":" + RegistryTLSMount + "/tls.key",
		"docker cp " + got + "/.sans " + tlsVolIO + ":" + RegistryTLSMount + "/.sans",
		"docker rm -f " + tlsVolIO,
	}
	pos := 0
	for _, want := range wantSeq {
		i := slices.Index(fd.log[pos:], want)
		if i < 0 {
			t.Fatalf("docker log lacks %q after position %d:\n%s", want, pos, strings.Join(fd.log, "\n"))
		}
		pos += i + 1
	}
}

func TestRegistriesTLSCertUpToDateSkipsTheMint(t *testing.T) {
	d, runner, fd, errBuf, p, _ := tlsDriver(t, "true")
	_, rec := stubSecretPlugin(t, runner, p.Base)
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	rec.Manifest = "" // detector: did the plugin run again?
	fd.log = nil
	d.tlsCrt = nil // a new process: the cert must come from the volume

	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	if rec.Manifest != "" {
		t.Fatal("plugin re-invoked although the SAN set was unchanged")
	}
	if slices.Contains(fd.log, "docker volume create "+tlsVol) {
		t.Fatal("volume re-created on an up-to-date run")
	}
	// The read-out: tls.crt and .sans come out of the volume as tar
	// streams through the throwaway container; tls.key is checked for
	// presence only (its stream is scanned for the entry and discarded:
	// nothing on the driver holds it).
	for _, want := range []string{
		"docker cp " + tlsVolIO + ":" + RegistryTLSMount + "/tls.crt -",
		"docker cp " + tlsVolIO + ":" + RegistryTLSMount + "/tls.key -",
		"docker cp " + tlsVolIO + ":" + RegistryTLSMount + "/.sans -",
	} {
		if !slices.Contains(fd.log, want) {
			t.Fatalf("%q not read from the volume:\n%s", want, strings.Join(fd.log, "\n"))
		}
	}
	if string(d.tlsCrt) != "FAKECRT" {
		t.Fatalf("cert read from the volume = %q", d.tlsCrt)
	}
}

// A volume with tls.crt but no tls.key is no certificate: the mint runs
// again into it, and the registries refuse to start on it.
func TestRegistriesTLSCertHalfPopulatedVolumeRemints(t *testing.T) {
	d, runner, fd, errBuf, p, cy := tlsDriver(t, "true")
	_, rec := stubSecretPlugin(t, runner, p.Base)
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(fd.volumePath(tlsVol), "tls.key"))
	d.tlsCrt = nil

	var out, vErr bytes.Buffer
	if err := d.registries(t.Context(), &out, &vErr, tlsDomain, cy); err == nil {
		t.Fatal("registries started on a volume without tls.key")
	}
	if !strings.Contains(vErr.String(), "holds no complete tls.crt + tls.key pair") {
		t.Fatalf("wrong error:\n%s", vErr.String())
	}

	rec.Manifest = ""
	fd.log = nil
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	if rec.Manifest == "" {
		t.Fatal("a half-populated volume did not re-mint")
	}
	if slices.Contains(fd.log, "docker volume create "+tlsVol) {
		t.Fatal("the existing volume was re-created")
	}
	if _, ok := fd.volumeFile(tlsVol, "tls.key"); !ok {
		t.Fatal("the re-mint left the volume without tls.key")
	}
}

// A docker failure on the read-out is a docker failure, not "no cert":
// the mint, the registries and `tls status` all stop on it and name it.
func TestRegistriesTLSReadSurfacesTheContainerCreateError(t *testing.T) {
	d, runner, fd, errBuf, p, cy := tlsDriver(t, "true")
	stubSecretPlugin(t, runner, p.Base)
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	d.tlsCrt = nil
	fd.wrap = func(c execx.Cmd) (bool, error) {
		if len(c.Args) >= 2 && c.Args[0] == "container" && c.Args[1] == "create" {
			writeErr(c, "Error response from daemon: no such image: registry:2.8.3\n")
			return true, fmt.Errorf("exit 1")
		}
		return false, nil
	}

	var vErr bytes.Buffer
	if err := d.registriesTLSCert(t.Context(), tlsDomain, &vErr); err == nil {
		t.Fatal("the mint treated a docker failure as success")
	}
	if !strings.Contains(vErr.String(), "error: docker container create "+tlsVolIO+" (volume "+tlsVol+") failed: Error response from daemon: no such image") {
		t.Fatalf("mint error:\n%s", vErr.String())
	}
	vErr.Reset()
	var out bytes.Buffer
	if err := d.registries(t.Context(), &out, &vErr, tlsDomain, cy); err == nil {
		t.Fatal("registries started on a docker failure")
	}
	if strings.Contains(vErr.String(), "holds no complete") || !strings.Contains(vErr.String(), "docker container create") {
		t.Fatalf("registries error:\n%s", vErr.String())
	}
	vErr.Reset()
	out.Reset()
	if err := d.RegistryTLSStatus(t.Context(), tlsDomain, &out, &vErr); err == nil {
		t.Fatal("status reported success on a docker failure")
	}
	if strings.Contains(out.String(), "certificate   none") || !strings.Contains(vErr.String(), "docker container create") {
		t.Fatalf("status:\n%s\n%s", out.String(), vErr.String())
	}
}

// A mint killed mid-way leaves clusters/<domain>/.registry-tls-tmp.* behind
// (with the key in it); every mint sweeps the stale ones first.
func TestRegistriesTLSMintSweepsStaleScratchDirs(t *testing.T) {
	d, runner, _, errBuf, p, _ := tlsDriver(t, "true")
	stubSecretPlugin(t, runner, p.Base)
	stale := filepath.Join(p.Clusters, tlsDomain, registryTLSScratchPrefix+"abandoned")
	testutil.WriteFile(t, filepath.Join(stale, "tls.key"), "OLDKEY")
	// A FILE with the prefix is not a scratch dir: left alone (the bash sweep
	// removes directories only).
	note := filepath.Join(p.Clusters, tlsDomain, registryTLSScratchPrefix+"note")
	testutil.WriteFile(t, note, "keep")

	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatalf("registriesTLSCert: %v\n%s", err, errBuf.String())
	}
	if fsutil.DirExists(stale) {
		t.Fatal("the stale scratch dir survived the mint")
	}
	if !fsutil.FileExists(note) {
		t.Fatal("the sweep removed a file that only shares the prefix")
	}
	os.Remove(note)
	if left := scratchDirs(t, p); len(left) != 0 {
		t.Fatalf("scratch dirs left behind: %v", left)
	}
}

// Half a legacy pair is no pair: one [warn] names it, nothing is imported,
// a fresh cert is minted.
func TestRegistriesTLSCertIncompleteLegacyPairWarnsAndMints(t *testing.T) {
	d, runner, fd, errBuf, p, _ := tlsDriver(t, "true")
	_, rec := stubSecretPlugin(t, runner, p.Base)
	legacy := filepath.Join(p.Base, ".secrets", "tls", "registries")
	testutil.WriteFile(t, filepath.Join(legacy, "tls.crt"), "LEGACYCRT")

	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatalf("registriesTLSCert: %v\n%s", err, errBuf.String())
	}
	want := "registry TLS: the legacy directory " + legacy + " holds an incomplete pair (tls.crt or tls.key is missing). Minting a fresh certificate."
	if strings.Count(errBuf.String(), "[warn]") != 1 || !strings.Contains(errBuf.String(), want) {
		t.Fatalf("want one [warn] %q, got:\n%s", want, errBuf.String())
	}
	if rec.Manifest == "" {
		t.Fatal("no fresh mint after the incomplete pair")
	}
	if got, _ := fd.volumeFile(tlsVol, "tls.crt"); got != "FAKECRT" {
		t.Fatalf("volume tls.crt = %q, want the fresh mint", got)
	}
	if slices.ContainsFunc(fd.log, func(l string) bool { return strings.HasPrefix(l, "docker cp -L ") }) {
		t.Fatalf("something was imported from the incomplete pair:\n%s", strings.Join(fd.log, "\n"))
	}
}

func TestRegistriesTLSCertSANChangeRemints(t *testing.T) {
	d, runner, fd, errBuf, p, _ := tlsDriver(t, "true")
	_, rec := stubSecretPlugin(t, runner, p.Base)
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	// The volume was minted for another SAN set (a mirror added since).
	os.WriteFile(filepath.Join(fd.volumePath(tlsVol), ".sans"), []byte("lok8s.local\n"), 0o644)
	rec.Manifest = ""
	rec.Crt = "FAKECRT2"
	fd.log = nil

	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	if rec.Manifest == "" {
		t.Fatal("changed SAN set did not re-mint")
	}
	if slices.Contains(fd.log, "docker volume create "+tlsVol) {
		t.Fatal("existing volume re-created on a re-mint")
	}
	if got, _ := fd.volumeFile(tlsVol, "tls.crt"); got != "FAKECRT2" {
		t.Fatalf("volume tls.crt = %q after the re-mint", got)
	}
}

func TestRegistriesTLSCertNoopWhenTLSDisabled(t *testing.T) {
	d, _, fd, errBuf, _, _ := tlsDriver(t, "false")
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	if fd.volumeExists(tlsVol) || len(fd.log) != 0 {
		t.Fatalf("plain mode touched docker:\n%s", strings.Join(fd.log, "\n"))
	}
}

func TestRegistriesTLSCertFailsFastWhenPluginMissing(t *testing.T) {
	d, _, fd, _, p, _ := tlsDriver(t, "true")
	t.Setenv(render.ModeEnv, string(render.ModeExec)) // only the exec pipeline needs the binary
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", filepath.Join(p.Base, ".kustomize-empty"))

	var vErr bytes.Buffer
	if err := d.registriesTLSCert(t.Context(), tlsDomain, &vErr); err == nil {
		t.Fatal("missing plugin reconciled as success")
	}
	if !strings.Contains(vErr.String(), "Secret plugin is not built") {
		t.Fatalf("wrong error:\n%s", vErr.String())
	}
	if fd.volumeExists(tlsVol) {
		t.Fatal("volume created although nothing could be minted")
	}
}

// TestRegistriesTLSCertImportsTheLegacyDirectory: a project that minted
// before v0.4.0 holds the pair under <Base>/.secrets/tls/registries. The
// first run creates the volume from those files byte for byte, warns once
// that the running containers still mount the directory, and leaves the
// files alone; the generator is not invoked when the SAN set still fits.
func TestRegistriesTLSCertImportsTheLegacyDirectory(t *testing.T) {
	d, runner, fd, errBuf, p, _ := tlsDriver(t, "true")
	_, rec := stubSecretPlugin(t, runner, p.Base)
	rf, err := regFile()
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(p.Base, ".secrets", "tls", "registries")
	testutil.WriteFile(t, filepath.Join(legacy, "tls.crt"), "LEGACYCRT")
	testutil.WriteFile(t, filepath.Join(legacy, "tls.key"), "LEGACYKEY")
	testutil.WriteFile(t, filepath.Join(legacy, ".sans"), strings.Join(registryTLSSANs(rf), "\n")+"\n")

	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatalf("registriesTLSCert: %v\n%s", err, errBuf.String())
	}
	if rec.Manifest != "" {
		t.Fatal("the generator ran although the legacy pair was imported")
	}
	for file, want := range map[string]string{"tls.crt": "LEGACYCRT", "tls.key": "LEGACYKEY"} {
		if got, ok := fd.volumeFile(tlsVol, file); !ok || got != want {
			t.Fatalf("imported %s = %q, %v; want %q", file, got, ok, want)
		}
	}
	// -L: a symlinked legacy file copies its target, not the link.
	if !slices.Contains(fd.log, "docker cp -L "+filepath.Join(legacy, "tls.key")+" "+tlsVolIO+":"+RegistryTLSMount+"/tls.key") {
		t.Fatalf("legacy key not copied (with -L) into the volume:\n%s", strings.Join(fd.log, "\n"))
	}
	for _, l := range fd.log {
		if strings.HasPrefix(l, "docker cp ") && strings.Contains(l, legacy) && !strings.HasPrefix(l, "docker cp -L ") {
			t.Fatalf("legacy file copied without -L: %s", l)
		}
	}
	warns := strings.Count(errBuf.String(), "[warn]")
	if warns != 1 || !strings.Contains(errBuf.String(), legacy) ||
		!strings.Contains(errBuf.String(), "lo registry down && lo registry up") {
		t.Fatalf("want exactly one [warn] naming %s and the recreate command, got %d:\n%s", legacy, warns, errBuf.String())
	}
	if !fsutil.FileExists(filepath.Join(legacy, "tls.key")) {
		t.Fatal("the legacy files were removed")
	}
	if string(d.tlsCrt) != "LEGACYCRT" {
		t.Fatalf("cert after import = %q", d.tlsCrt)
	}

	// A second run finds the volume and imports nothing again.
	errBuf.Reset()
	fd.log = nil
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errBuf.String(), "[warn]") || slices.Contains(fd.log, "docker volume create "+tlsVol) {
		t.Fatalf("second run imported again:\n%s\n%s", errBuf.String(), strings.Join(fd.log, "\n"))
	}
}

func TestRegistriesTLSCertImportWithStaleSANsRemints(t *testing.T) {
	d, runner, fd, errBuf, p, _ := tlsDriver(t, "true")
	_, rec := stubSecretPlugin(t, runner, p.Base)
	legacy := filepath.Join(p.Base, ".secrets", "tls", "registries")
	testutil.WriteFile(t, filepath.Join(legacy, "tls.crt"), "LEGACYCRT")
	testutil.WriteFile(t, filepath.Join(legacy, "tls.key"), "LEGACYKEY")
	// No .sans: the SAN set the legacy pair was minted for is unknown.

	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatalf("registriesTLSCert: %v\n%s", err, errBuf.String())
	}
	if rec.Manifest == "" {
		t.Fatal("an import without a SAN record must re-mint")
	}
	if got, _ := fd.volumeFile(tlsVol, "tls.crt"); got != "FAKECRT" {
		t.Fatalf("volume tls.crt = %q, want the fresh mint", got)
	}
	if n := strings.Count(strings.Join(fd.log, "\n"), "docker volume create "+tlsVol); n != 1 {
		t.Fatalf("volume created %d times", n)
	}
}

// TestRegistriesMountTheVolume: every registry container of the set (the
// project's build/cache and the shared mirror) mounts the volume read-only
// at the cert path; no host directory appears in the run argv. The cert
// content is in the config hash, so a re-minted cert recreates them.
func TestRegistriesMountTheVolume(t *testing.T) {
	d, runner, fd, errBuf, p, cy := tlsDriver(t, "true")
	_, rec := stubSecretPlugin(t, runner, p.Base)
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := d.registries(t.Context(), &out, errBuf, tlsDomain, cy); err != nil {
		t.Fatalf("registries: %v\n%s", err, errBuf.String())
	}
	runs := 0
	for _, l := range fd.log {
		if !strings.HasPrefix(l, "docker run ") {
			continue
		}
		runs++
		if !strings.Contains(l, " --volume "+tlsVol+":"+RegistryTLSMount+":ro ") {
			t.Errorf("registry run without the volume mount: %s", l)
		}
		if strings.Contains(l, ".secrets") {
			t.Errorf("registry run mounts a project directory: %s", l)
		}
	}
	if runs != 3 {
		t.Fatalf("%d registry runs, want 3:\n%s", runs, strings.Join(fd.log, "\n"))
	}
	for _, name := range []string{"lok8s-registry-build", "lok8s-registry-cache", "lok8s-registry-io-docker"} {
		if got := fd.containerMount(name); got != "volume|"+tlsVol {
			t.Errorf("%s mounts %q", name, got)
		}
	}

	// A new cert (renewed, or re-minted for a new SAN set) changes the
	// hash: the containers are recreated with it.
	rec.Crt = "FAKECRT2"
	os.WriteFile(filepath.Join(fd.volumePath(tlsVol), ".sans"), []byte("stale\n"), 0o644)
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := d.registries(t.Context(), &out, errBuf, tlsDomain, cy); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "registry/lok8s-registry-build configured") {
		t.Fatalf("re-minted cert did not recreate the containers:\n%s", out.String())
	}
}

func TestRegistriesFailWithoutACertInTheVolume(t *testing.T) {
	d, _, fd, _, _, cy := tlsDriver(t, "true")
	var out, vErr bytes.Buffer
	if err := d.registries(t.Context(), &out, &vErr, tlsDomain, cy); err == nil {
		t.Fatal("TLS registries started without a cert")
	}
	if !strings.Contains(vErr.String(), "volume "+tlsVol+" holds no complete tls.crt + tls.key pair") {
		t.Fatalf("wrong error:\n%s", vErr.String())
	}
	for _, l := range fd.log {
		if strings.HasPrefix(l, "docker run ") {
			t.Fatalf("a registry was started anyway: %s", l)
		}
	}
}

// TestRegistriesTLSCertMintsInProcess drives the DEFAULT pipeline: no
// plugin binary, no KUSTOMIZE_PLUGIN_HOME, no exec besides docker — the
// imported secrets.lok8s.dev generator mints the leaf against a throwaway
// CAROOT (created on demand, like the dev CA) into the volume. The pair in
// the volume must be real: a leaf signed by that CA whose SANs are exactly
// the registry hostnames + IPs, with the key matching the cert; the
// scratch store is gone and no project store was written.
func TestRegistriesTLSCertMintsInProcess(t *testing.T) {
	d, runner, fd, errBuf, p, _ := tlsDriver(t, "true")
	t.Setenv(render.ModeEnv, "")
	t.Setenv("CAROOT", filepath.Join(p.Base, "caroot"))
	inner := runner.handler
	runner.handler = func(c execx.Cmd) error {
		if c.Name != "docker" {
			t.Fatalf("in-process mint must exec nothing but docker, ran %s %v", c.Name, c.Args)
		}
		return inner(c)
	}

	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatalf("registriesTLSCert: %v\n%s", err, errBuf.String())
	}
	crtPEM, ok := fd.volumeFile(tlsVol, "tls.crt")
	if !ok {
		t.Fatal("no tls.crt in the volume")
	}
	keyPEM, ok := fd.volumeFile(tlsVol, "tls.key")
	if !ok {
		t.Fatal("no tls.key in the volume")
	}
	pair, err := tls.X509KeyPair([]byte(crtPEM), []byte(keyPEM))
	if err != nil {
		t.Fatalf("tls.crt/tls.key are not a matching pair: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA {
		t.Fatal("minted a CA, not a leaf")
	}
	for _, want := range []string{"lok8s.local", "lok8s.cache", "docker.io"} {
		if !slices.Contains(leaf.DNSNames, want) {
			t.Errorf("DNS SAN %q missing (have %v)", want, leaf.DNSNames)
		}
	}
	for _, want := range []string{"10.125.50.101", "10.125.50.102"} {
		found := false
		for _, ip := range leaf.IPAddresses {
			found = found || ip.String() == want
		}
		if !found {
			t.Errorf("IP SAN %q missing (have %v)", want, leaf.IPAddresses)
		}
	}
	caPEM, err := os.ReadFile(filepath.Join(p.Base, "caroot", "rootCA.pem"))
	if err != nil {
		t.Fatalf("CAROOT CA not created: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("rootCA.pem unparsable")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "lok8s.local",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("leaf does not verify against the CAROOT CA: %v", err)
	}
	if left := scratchDirs(t, p); len(left) != 0 {
		t.Fatalf("scratch dirs left behind: %v", left)
	}
	if fsutil.DirExists(filepath.Join(p.Base, ".secrets")) {
		t.Fatal("the in-process mint wrote into <Base>/.secrets")
	}
	if !bytes.Equal(d.tlsCrt, []byte(crtPEM)) {
		t.Fatal("the driver's cached cert is not the one in the volume")
	}

	// Idempotent: the same SAN set does not re-mint.
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	if again, _ := fd.volumeFile(tlsVol, "tls.crt"); again != crtPEM {
		t.Fatal("unchanged SAN set re-minted the cert")
	}
}

func TestRegistryTLSStatusShowsTheCertAndTheMounts(t *testing.T) {
	d, _, fd, errBuf, p, cy := tlsDriver(t, "true")
	t.Setenv(render.ModeEnv, "")
	t.Setenv("CAROOT", filepath.Join(p.Base, "caroot"))
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	var sink bytes.Buffer
	if err := d.registries(t.Context(), &sink, errBuf, tlsDomain, cy); err != nil {
		t.Fatal(err)
	}
	// One container of the set still runs with the pre-v0.4.0 bind mount.
	fd.setContainerMount("lok8s-registry-cache", "bind|/home/me/proj/.secrets/tls/registries")

	var out bytes.Buffer
	if err := d.RegistryTLSStatus(t.Context(), tlsDomain, &out, errBuf); err != nil {
		t.Fatalf("status: %v\n%s", err, errBuf.String())
	}
	for _, want := range []string{
		"registry TLS  on",
		"volume        " + tlsVol,
		"not before    20",
		"not after     20",
		"sans          lok8s.local lok8s.cache docker.io 10.125.50.101 10.125.50.102 10.125.200.2\n",
		"lok8s-registry-build         volume " + tlsVol,
		"lok8s-registry-cache         bind /home/me/proj/.secrets/tls/registries (legacy). Next: lo registry down && lo registry up",
		"lok8s-registry-io-docker     volume " + tlsVol,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "san set       stale") {
		t.Fatalf("fresh mint reported stale:\n%s", out.String())
	}

	// A stale SAN record and an absent container are named as such.
	os.WriteFile(filepath.Join(fd.volumePath(tlsVol), ".sans"), []byte("lok8s.local\n"), 0o644)
	os.Remove(fd.containerPath("lok8s-registry-io-docker"))
	out.Reset()
	if err := d.RegistryTLSStatus(t.Context(), tlsDomain, &out, errBuf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"san set       stale. The registries changed since the mint. Next: lo registry tls renew",
		"lok8s-registry-io-docker     absent",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
}

func TestRegistryTLSStatusWithoutAVolume(t *testing.T) {
	d, _, fd, errBuf, _, _ := tlsDriver(t, "true")
	var out bytes.Buffer
	if err := d.RegistryTLSStatus(t.Context(), tlsDomain, &out, errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "certificate   none. The volume does not exist. Next: lo up") {
		t.Fatalf("status:\n%s", out.String())
	}
	// An existing volume without the pair.
	fd.createVolume(execx.Cmd{}, []string{tlsVol})
	out.Reset()
	if err := d.RegistryTLSStatus(t.Context(), tlsDomain, &out, errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "certificate   none. The volume holds no complete tls.crt + tls.key pair. Next: lo registry tls renew") {
		t.Fatalf("status:\n%s", out.String())
	}
}

func TestRegistryTLSStatusPlainMode(t *testing.T) {
	d, _, _, errBuf, _, _ := tlsDriver(t, "false")
	var out bytes.Buffer
	if err := d.RegistryTLSStatus(t.Context(), tlsDomain, &out, errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "registry TLS  off (spec.registries.tls: false in test.lok8s.dev)") {
		t.Fatalf("status:\n%s", out.String())
	}
}

func TestRegistryTLSRenewRemintsAndRestartsTheSet(t *testing.T) {
	d, runner, fd, errBuf, p, cy := tlsDriver(t, "true")
	_, rec := stubSecretPlugin(t, runner, p.Base)
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	var sink bytes.Buffer
	if err := d.registries(t.Context(), &sink, errBuf, tlsDomain, cy); err != nil {
		t.Fatal(err)
	}
	os.Remove(fd.containerPath("lok8s-registry-io-docker")) // one container of the set is absent
	rec.Manifest = ""
	rec.Crt = "RENEWED"
	fd.log = nil

	var out bytes.Buffer
	if err := d.RegistryTLSRenew(t.Context(), tlsDomain, &out, errBuf); err != nil {
		t.Fatalf("renew: %v\n%s", err, errBuf.String())
	}
	if rec.Manifest == "" {
		t.Fatal("renew did not mint")
	}
	if got, _ := fd.volumeFile(tlsVol, "tls.crt"); got != "RENEWED" {
		t.Fatalf("volume tls.crt = %q after renew", got)
	}
	if slices.Contains(fd.log, "docker volume create "+tlsVol) {
		t.Fatal("renew re-created the existing volume")
	}
	for _, want := range []string{
		"registry TLS renewed: volume " + tlsVol + ", 6 SANs",
		"restarted lok8s-registry-build",
		"restarted lok8s-registry-cache",
		"Next: run 'lo up' in each cluster that uses the set.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("renew output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "restarted lok8s-registry-io-docker") ||
		slices.Contains(fd.log, "docker restart lok8s-registry-io-docker") {
		t.Fatal("renew restarted an absent container")
	}
	for _, name := range []string{"lok8s-registry-build", "lok8s-registry-cache"} {
		if !slices.Contains(fd.log, "docker restart "+name) {
			t.Fatalf("%s not restarted:\n%s", name, strings.Join(fd.log, "\n"))
		}
	}
}

func TestRegistryTLSRenewRefusesInPlainMode(t *testing.T) {
	d, _, fd, _, _, _ := tlsDriver(t, "false")
	var out, vErr bytes.Buffer
	if err := d.RegistryTLSRenew(t.Context(), tlsDomain, &out, &vErr); err == nil {
		t.Fatal("renew succeeded with TLS off")
	}
	if !strings.Contains(vErr.String(), "error: spec.registries.tls is false in test.lok8s.dev. There is no certificate to renew.") {
		t.Fatalf("wrong error:\n%s", vErr.String())
	}
	if len(fd.log) != 0 {
		t.Fatalf("renew touched docker with TLS off:\n%s", strings.Join(fd.log, "\n"))
	}
}

func TestRegistryTLSDoctorLine(t *testing.T) {
	d, runner, fd, errBuf, p, cy := tlsDriver(t, "true")
	stubSecretPlugin(t, runner, p.Base)

	// No containers yet: nothing to say (doctor stays byte-identical).
	if line, _ := d.RegistryTLSDoctor(t.Context(), tlsDomain); line != "" {
		t.Fatalf("line without containers: %q", line)
	}
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	var sink bytes.Buffer
	if err := d.registries(t.Context(), &sink, errBuf, tlsDomain, cy); err != nil {
		t.Fatal(err)
	}
	line, warn := d.RegistryTLSDoctor(t.Context(), tlsDomain)
	if warn || line != "registry TLS: 3 containers mount volume "+tlsVol {
		t.Fatalf("line = %q, warn = %v", line, warn)
	}

	fd.setContainerMount("lok8s-registry-build", "bind|/home/me/proj/.secrets/tls/registries")
	line, warn = d.RegistryTLSDoctor(t.Context(), tlsDomain)
	if !warn || line != "registry TLS: 1 of 3 containers mount the legacy directory /home/me/proj/.secrets/tls/registries. Next: lo registry down && lo registry up" {
		t.Fatalf("line = %q, warn = %v", line, warn)
	}
}

func TestCleanupRemovesTheTLSVolume(t *testing.T) {
	d, runner, fd, errBuf, p, _ := tlsDriver(t, "true")
	stubSecretPlugin(t, runner, p.Base)
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	d.cleanupRegistries(t.Context(), "test-tls")
	if fd.volumeExists(tlsVol) {
		t.Fatal("clean left the cert volume")
	}
	if d.tlsCrt != nil {
		t.Fatal("clean kept the cached cert")
	}
}

func TestTarFirstFile(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "certs", Mode: 0o755, Typeflag: tar.TypeDir})
	tw.WriteHeader(&tar.Header{Name: "certs/tls.crt", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg})
	tw.Write([]byte("PEM"))
	tw.Close()
	if got, ok := tarFirstFile(buf.Bytes()); !ok || string(got) != "PEM" {
		t.Fatalf("tarFirstFile = %q, %v", got, ok)
	}
	if _, ok := tarFirstFile(nil); ok {
		t.Fatal("empty stream reported a file")
	}
	if _, ok := tarFirstFile([]byte("not a tar")); ok {
		t.Fatal("garbage reported a file")
	}
}
