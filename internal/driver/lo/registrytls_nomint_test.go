package lo

// registrytls_nomint_test.go: with LOK8S_SECRETS_NO_MINT set, the exec
// pipeline of the registry TLS mint (LO_RENDER=exec) refuses a Secret
// plugin that does not have the no-mint run before the mint
// (build.CheckNoMintPlugin). An older plugin ignores the switch and mints:
// the leaf, and a CA in CAROOT when there is none. The in-process
// generator honours the switch itself. bats: "lo::registry_tls_mint with
// no-mint refuses a Secret plugin without the no-mint run" in
// tests/unit/registry_tls_test.bats.

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

// noMintProbe answers the probe of the stub plugin (stubSecretPlugin) with
// caps and counts it, and counts each mint (every other run of the
// plugin), which the stub answers.
func noMintProbe(t *testing.T, runner *fakeRunner, pluginBin, caps string) (probes, mints *int) {
	t.Helper()
	probes, mints = new(int), new(int)
	inner := runner.handler
	runner.handler = func(c execx.Cmd) error {
		if c.Name == pluginBin {
			if slices.Equal(c.Args, []string{"--capabilities"}) {
				*probes++
				writeOut(c, caps)
				return nil
			}
			*mints++
		}
		return inner(c)
	}
	t.Setenv("TMPDIR", t.TempDir())
	return probes, mints
}

func TestRegistriesTLSMintChecksTheNoMintPlugin(t *testing.T) {
	for _, tc := range []struct {
		name, caps string
		ok         bool
	}{
		{"without the no-mint run", "dry-run\nkeys-only\n", false},
		{"no capabilities", "", false},
		{"with the no-mint run", "dry-run\nkeys-only\nno-mint\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, runner, fd, _, p, _ := tlsDriver(t, "true")
			pluginBin, rec := stubSecretPlugin(t, runner, p.Base)
			probes, mints := noMintProbe(t, runner, pluginBin, tc.caps)
			t.Setenv(secret.NoMintEnv, "1")

			var errOut bytes.Buffer
			err := d.registriesTLSCert(t.Context(), tlsDomain, &errOut)
			if *probes != 1 {
				t.Errorf("probes = %d, want 1", *probes)
			}
			if tc.ok {
				if err != nil {
					t.Fatalf("registriesTLSCert: %v\n%s", err, errOut.String())
				}
				if *mints != 1 || !strings.Contains(rec.Manifest, "kind: Secret") {
					t.Errorf("mints = %d, manifest %q: want one mint", *mints, rec.Manifest)
				}
				return
			}
			if !errors.Is(err, ui.ErrHandled) {
				t.Fatalf("registriesTLSCert = %v, want the handled error\n%s", err, errOut.String())
			}
			want := "[error] the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret does not have the no-mint run (LOK8S_SECRETS_NO_MINT is set, and an older plugin ignores it and generates values): run 'lo toolchain install' to install the plugin of this lo\n"
			if errOut.String() != want {
				t.Errorf("stderr = %q\nwant %q", errOut.String(), want)
			}
			if *mints != 0 {
				t.Errorf("the plugin minted %d times", *mints)
			}
			if fd.volumeExists(tlsVol) {
				t.Error("the volume was created although nothing was minted")
			}
			if left := scratchDirs(t, p); len(left) != 0 {
				t.Errorf("scratch dirs left behind: %v", left)
			}
		})
	}
}

// With the switch off, the plugin is not probed and mints as before.
func TestRegistriesTLSMintNoMintOffDoesNotProbe(t *testing.T) {
	d, runner, _, _, p, _ := tlsDriver(t, "true")
	pluginBin, _ := stubSecretPlugin(t, runner, p.Base)
	probes, mints := noMintProbe(t, runner, pluginBin, "dry-run\nkeys-only\n")
	t.Setenv(secret.NoMintEnv, "false")
	var errOut bytes.Buffer
	if err := d.registriesTLSCert(t.Context(), tlsDomain, &errOut); err != nil {
		t.Fatalf("registriesTLSCert: %v\n%s", err, errOut.String())
	}
	if *probes != 0 || *mints != 1 {
		t.Errorf("probes = %d, mints = %d; want 0 and 1", *probes, *mints)
	}
}

// `lo registry tls renew` mints through the same check: with the switch on,
// a plugin without the no-mint run is refused before the mint. The volume
// keeps its certificate, and no container of the set restarts.
func TestRegistryTLSRenewChecksTheNoMintPlugin(t *testing.T) {
	d, runner, fd, errBuf, p, cy := tlsDriver(t, "true")
	pluginBin, rec := stubSecretPlugin(t, runner, p.Base)
	if err := d.registriesTLSCert(t.Context(), tlsDomain, errBuf); err != nil {
		t.Fatal(err)
	}
	var sink bytes.Buffer
	if err := d.registries(t.Context(), &sink, errBuf, tlsDomain, cy); err != nil {
		t.Fatal(err)
	}
	before, ok := fd.volumeFile(tlsVol, "tls.crt")
	if !ok {
		t.Fatal("the first mint wrote no tls.crt")
	}
	probes, mints := noMintProbe(t, runner, pluginBin, "dry-run\nkeys-only\n")
	t.Setenv(secret.NoMintEnv, "1")
	rec.Manifest = ""
	rec.Crt = fakePEM("CERTIFICATE", "RENEWED")
	fd.log = nil

	var out, errOut bytes.Buffer
	err := d.RegistryTLSRenew(t.Context(), tlsDomain, &out, &errOut)
	if !errors.Is(err, ui.ErrHandled) {
		t.Fatalf("RegistryTLSRenew = %v, want the handled error\n%s", err, errOut.String())
	}
	want := "[error] the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret does not have the no-mint run (LOK8S_SECRETS_NO_MINT is set, and an older plugin ignores it and generates values): run 'lo toolchain install' to install the plugin of this lo\n"
	if errOut.String() != want {
		t.Errorf("stderr = %q\nwant %q", errOut.String(), want)
	}
	if *probes != 1 || *mints != 0 {
		t.Errorf("probes = %d, mints = %d; want 1 and 0", *probes, *mints)
	}
	if got, _ := fd.volumeFile(tlsVol, "tls.crt"); got != before {
		t.Errorf("volume tls.crt = %q after a refused renew, want the old certificate", got)
	}
	for _, l := range fd.log {
		if strings.HasPrefix(l, "docker restart ") {
			t.Errorf("a refused renew restarted a container: %s", l)
		}
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", out.String())
	}
}
