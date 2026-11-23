package secret

// nomint_test.go: the no-mint run (LOK8S_SECRETS_NO_MINT) end to end
// through secret.Run, against fixture stores under t.TempDir() only. With
// the switch on, a value that the run would generate or write fails the
// run with the names of the entries, and nothing is written or emitted.

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noMintStore makes a store that holds the entry KEEP of the Secret db in
// the namespace apps: its plaintext and its .enc twin, as `lo secrets
// decrypt` leaves them.
func noMintStore(t *testing.T) string {
	t.Helper()
	dir := encStore(t, "Secret.db.apps.KEEP")
	if err := os.WriteFile(filepath.Join(dir, "Secret.db.apps.KEEP"), []byte("kept-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// noMintSpec reads KEEP from the store and generates NEW (passwd) and
// SIGN (key): two entries in two generators.
const noMintSpec = `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: db, namespace: apps}
passwd:
  KEEP: 32
  NEW: 32
key:
  SIGN: {algorithm: ed25519}
`

const noMintNext = "\nPut the values in the store (lo secrets set, or lo secrets decrypt), then build again. Or build without LOK8S_SECRETS_NO_MINT."

// With the switch on, the run fails after every generator ran and names
// each entry that it would generate, by namespace, Secret and key: NEW
// (passwd) and SIGN (key), never KEEP, which the store holds. The text
// holds no value and no length. Nothing is emitted, and the store is as it
// was (names, modes, sizes and times).
func TestNoMint_NamesEveryEntryThatTheRunWouldGenerate(t *testing.T) {
	dir := noMintStore(t)
	before := storeSnapshot(t, dir)
	out, err := runEntry(t, []byte(noMintSpec), map[string]string{PathSecretsEnv: dir, NoMintEnv: "1"})
	nerr, ok := errors.AsType[*NoMintError](err)
	if !ok {
		t.Fatalf("err = %v, want a *NoMintError", err)
	}
	want := `LOK8S_SECRETS_NO_MINT is on, so the plugin writes nothing, but this run must write 2 values:
  - namespace "apps", Secret "db", key "NEW": the store has no value, so the run would generate one
  - namespace "apps", Secret "db", key "SIGN": the store has no value, so the run would generate one` + noMintNext
	if err.Error() != want {
		t.Errorf("error:\n%s\nwant:\n%s", err, want)
	}
	if nerr.Cause != nil {
		t.Errorf("Cause = %v, want nil", nerr.Cause)
	}
	if out != "" {
		t.Errorf("a no-mint run that fails emitted:\n%s", out)
	}
	if got := storeSnapshot(t, dir); !maps.Equal(got, before) {
		t.Errorf("the store changed:\nbefore %v\nafter  %v", before, got)
	}
}

// Without the switch, or with an off value, the run is as before: it
// generates NEW and SIGN into the store and emits KEEP from the store.
func TestNoMint_OffKeepsTheOldBehaviour(t *testing.T) {
	for name, set := range map[string]func(map[string]string){
		"unset":   func(map[string]string) {},
		"empty":   func(e map[string]string) { e[NoMintEnv] = "" },
		"0":       func(e map[string]string) { e[NoMintEnv] = "0" },
		"false":   func(e map[string]string) { e[NoMintEnv] = "false" },
		" FALSE ": func(e map[string]string) { e[NoMintEnv] = " FALSE " },
	} {
		t.Run(name, func(t *testing.T) {
			dir := noMintStore(t)
			env := map[string]string{PathSecretsEnv: dir}
			set(env)
			out, err := runEntry(t, []byte(noMintSpec), env)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := decodeSecretDataKey(t, out, "KEEP"); got != "kept-value" {
				t.Errorf("KEEP = %q, want the value of the store", got)
			}
			for _, entry := range []string{"Secret.db.apps.NEW", "Secret.db.apps.SIGN"} {
				if _, err := os.Stat(filepath.Join(dir, entry)); err != nil {
					t.Errorf("the run did not generate %s: %v", entry, err)
				}
			}
		})
	}
}

// The on values are those of LOK8S_SECRETS_DISABLE: "1" and "true", in
// any letter case, with spaces around them.
func TestNoMint_OnValues(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", " true "} {
		t.Run(v, func(t *testing.T) {
			_, err := runEntry(t, []byte(noMintSpec), map[string]string{PathSecretsEnv: noMintStore(t), NoMintEnv: v})
			if _, ok := errors.AsType[*NoMintError](err); !ok {
				t.Errorf("%s=%q: err = %v, want a *NoMintError", NoMintEnv, v, err)
			}
		})
	}
}

// A value that is not an on or an off value is an error before anything
// is read: a typo never turns the switch off. With LOK8S_SECRETS_DISABLE on
// too, the error stays.
func TestNoMint_UnknownValueFailsClosed(t *testing.T) {
	for _, v := range []string{"yes", "ture", "2", "   "} {
		for _, disable := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q, disable %v", v, disable), func(t *testing.T) {
				dir := noMintStore(t)
				before := storeSnapshot(t, dir)
				env := map[string]string{PathSecretsEnv: dir, NoMintEnv: v}
				if disable {
					env[DisableEnv] = "1"
				}
				out, err := runEntry(t, []byte(noMintSpec), env)
				want := NoMintEnv + `: unknown value "` + v + `" (want "1", "true", "0", "false" or unset)`
				if err == nil || err.Error() != want {
					t.Fatalf("disable=%v: err = %v, want %q", disable, err, want)
				}
				if out != "" {
					t.Errorf("disable=%v: emitted %q", disable, out)
				}
				if got := storeSnapshot(t, dir); !maps.Equal(got, before) {
					t.Errorf("disable=%v: the store changed", disable)
				}
			})
		}
	}
}

// LOK8S_SECRETS_DISABLE wins over the switch: that run reads no store and
// generates nothing, so there is nothing to refuse. It emits nothing and
// succeeds, as without the switch. The control: in the same fixture, the
// switch without LOK8S_SECRETS_DISABLE refuses.
func TestNoMint_DisableWins(t *testing.T) {
	dir := noMintStore(t)
	before := storeSnapshot(t, dir)
	if _, err := runEntry(t, []byte(noMintSpec), map[string]string{PathSecretsEnv: dir, NoMintEnv: "1"}); err == nil {
		t.Fatal("the control: the switch without LOK8S_SECRETS_DISABLE did not refuse")
	} else if _, ok := errors.AsType[*NoMintError](err); !ok {
		t.Fatalf("the control: err = %v, want a *NoMintError", err)
	}
	out, err := runEntry(t, []byte(noMintSpec), map[string]string{PathSecretsEnv: dir, NoMintEnv: "1", DisableEnv: "1"})
	if err != nil || out != "" {
		t.Fatalf("out %q, err %v; want no output and no error", out, err)
	}
	if got := storeSnapshot(t, dir); !maps.Equal(got, before) {
		t.Errorf("the store changed")
	}
}

// An env: value that the store does not hold is a write: the switch
// refuses it. An update: true value equal to the stored one is no write,
// so that run emits the Secret.
func TestNoMint_RefusesAWriteOfAValueThatTheStoreDoesNotHold(t *testing.T) {
	const line = `namespace "default", Secret "api", key "TOKEN": the run would write a value that the store does not hold`
	cases := []struct {
		name, spec, stored string
		refused            bool
	}{
		{"env, not in the store", "env:\n  TOKEN: {var: API_TOKEN}\n", "", true},
		{"update, another value in the store", "env:\n  TOKEN: {var: API_TOKEN, update: true}\n", "old-value", true},
		{"update, the same value in the store", "env:\n  TOKEN: {var: API_TOKEN, update: true}\n", "from-env", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := encStore(t)
			if tc.stored != "" {
				if err := os.WriteFile(filepath.Join(dir, "Secret.api.default.TOKEN"), []byte(tc.stored), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := storeSnapshot(t, dir)
			spec := "apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata: {name: api, namespace: default}\n" + tc.spec
			out, err := runEntry(t, []byte(spec), map[string]string{PathSecretsEnv: dir, NoMintEnv: "1", "API_TOKEN": "from-env"})
			if tc.refused {
				want := "LOK8S_SECRETS_NO_MINT is on, so the plugin writes nothing, but this run must write 1 value:\n  - " + line + noMintNext
				if err == nil || err.Error() != want {
					t.Errorf("error:\n%v\nwant:\n%s", err, want)
				}
				if out != "" {
					t.Errorf("emitted %q", out)
				}
			} else {
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if got := decodeSecretDataKey(t, out, "TOKEN"); got != "from-env" {
					t.Errorf("TOKEN = %q, want from-env", got)
				}
			}
			if got := storeSnapshot(t, dir); !maps.Equal(got, before) {
				t.Errorf("the store changed")
			}
		})
	}
}

// When the store holds every value, a run with the switch emits what a run
// without it emits, and writes nothing.
func TestNoMint_EmitsWhatARunWithoutItEmits(t *testing.T) {
	dir := noMintStore(t)
	want, err := runEntry(t, []byte(noMintSpec), map[string]string{PathSecretsEnv: dir})
	if err != nil {
		t.Fatalf("the run without the switch: %v", err)
	}
	before := storeSnapshot(t, dir)
	got, err := runEntry(t, []byte(noMintSpec), map[string]string{PathSecretsEnv: dir, NoMintEnv: "1"})
	if err != nil {
		t.Fatalf("the run with the switch: %v", err)
	}
	if got != want {
		t.Errorf("with the switch:\n%s\nwithout it:\n%s", got, want)
	}
	if after := storeSnapshot(t, dir); !maps.Equal(after, before) {
		t.Errorf("the store changed")
	}
}

// A run that stops with an error after a refusal names the refusals too,
// and then the error. A secretRef miss is the error of a real run (it
// names no dry run).
func TestNoMint_AStoppedRunNamesTheRefusalsToo(t *testing.T) {
	dir := encStore(t)
	spec := `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: app, namespace: default}
passwd:
  NEW: 32
secretRef:
  REF: other/PASSWORD
`
	_, err := runEntry(t, []byte(spec), map[string]string{PathSecretsEnv: dir, NoMintEnv: "1"})
	nerr, ok := errors.AsType[*NoMintError](err)
	if !ok {
		t.Fatalf("err = %v, want a *NoMintError", err)
	}
	if nerr.Cause == nil || !strings.HasPrefix(nerr.Cause.Error(), "secretRef.REF: read other/default/PASSWORD: ") {
		t.Errorf("Cause = %v, want the secretRef miss", nerr.Cause)
	}
	msg := err.Error()
	for _, part := range []string{
		`  - namespace "default", Secret "app", key "NEW": the store has no value, so the run would generate one`,
		"\nThe run also stopped with this error: secretRef.REF: read other/default/PASSWORD: ",
	} {
		if !strings.Contains(msg, part) {
			t.Errorf("error has no %q:\n%s", part, msg)
		}
	}
	if strings.Contains(msg, "dry run") {
		t.Errorf("the secretRef error names a dry run:\n%s", msg)
	}
}

// A bash: entry that the store lacks does not run its command in a no-mint
// run: the run refuses the entry, the command leaves no marker, and no
// .sha or .bash-allow is written. Without the switch the same command runs.
func TestNoMint_DoesNotRunABashCommand(t *testing.T) {
	spec := func(marker string) []byte {
		return []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: hook, namespace: default}
bash:
  OUT: {exec: "touch '` + marker + `'; printf v"}
`)
	}
	control := filepath.Join(t.TempDir(), "control")
	if _, err := runEntry(t, spec(control), map[string]string{PathSecretsEnv: encStore(t)}); err != nil {
		t.Fatalf("the run without the switch: %v", err)
	}
	if _, err := os.Stat(control); err != nil {
		t.Fatalf("the command did not run without the switch: %v", err)
	}

	dir := encStore(t)
	before := storeSnapshot(t, dir)
	marker := filepath.Join(t.TempDir(), "marker")
	_, err := runEntry(t, spec(marker), map[string]string{PathSecretsEnv: dir, NoMintEnv: "1"})
	if err == nil || !strings.Contains(err.Error(), `namespace "default", Secret "hook", key "OUT": the store has no value`) {
		t.Errorf("err = %v, want the refusal of OUT", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the bash command ran in a no-mint run (stat: %v)", err)
	}
	if got := storeSnapshot(t, dir); !maps.Equal(got, before) {
		t.Errorf("the store changed:\nbefore %v\nafter  %v", before, got)
	}
}

// The switch wins over a dry run: the run fails on a value that it would
// generate. It writes no line to the report of the dry run and no value to
// its run store (LOK8S_BOOTSTRAP_DRYRUN_STORE).
func TestNoMint_WinsOverADryRun(t *testing.T) {
	report := filepath.Join(t.TempDir(), "report")
	if err := os.WriteFile(report, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := runEntry(t, []byte(noMintSpec), map[string]string{
		PathSecretsEnv:                  noMintStore(t),
		NoMintEnv:                       "1",
		"LOK8S_BOOTSTRAP_DRYRUN":        "1",
		"LOK8S_BOOTSTRAP_DRYRUN_REPORT": report,
		"LOK8S_BOOTSTRAP_DRYRUN_STORE":  run,
	})
	if _, ok := errors.AsType[*NoMintError](err); !ok {
		t.Errorf("err = %v, want a *NoMintError", err)
	}
	if got, _ := os.ReadFile(report); len(got) != 0 {
		t.Errorf("the dry-run report got:\n%s", got)
	}
	if got := listing(t, run); got != "" {
		t.Errorf("the run store got: %s", got)
	}
}

// A store directory that does not exist is not made: every entry is
// refused, and the directory stays absent.
func TestNoMint_DoesNotMakeTheStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "clusters", "alpha.dev", "secrets")
	_, err := runEntry(t, []byte(noMintSpec), map[string]string{PathSecretsEnv: dir, NoMintEnv: "1"})
	nerr, ok := errors.AsType[*NoMintError](err)
	if !ok || len(nerr.Refused) != 3 {
		t.Fatalf("err = %v, want a *NoMintError with KEEP, NEW and SIGN", err)
	}
	if _, err := os.Stat(filepath.Dir(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the run made %s (stat: %v)", filepath.Dir(dir), err)
	}
}

// A leaf signed by the CA in CAROOT: when CAROOT has no CA, the run would
// make one there. The switch refuses it with the leaf entries, and CAROOT
// stays empty.
func TestNoMint_RefusesTheCAInCAROOT(t *testing.T) {
	caroot := t.TempDir()
	t.Setenv("CAROOT", caroot)
	spec := `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: web, namespace: default}
type: kubernetes.io/tls
cert:
  hosts: [web.example.test]
`
	_, err := runEntry(t, []byte(spec), map[string]string{PathSecretsEnv: encStore(t), NoMintEnv: "1"})
	nerr, ok := errors.AsType[*NoMintError](err)
	if !ok {
		t.Fatalf("err = %v, want a *NoMintError", err)
	}
	if len(nerr.Refused) != 3 || !strings.Contains(err.Error(), "this run must write 3 values:") {
		t.Errorf("want 3 refusals (the CA, tls.key, tls.crt), got %+v:\n%s", nerr.Refused, err)
	}
	for _, part := range []string{
		"  - the CA in CAROOT " + caroot + ": CAROOT has no CA, so the run would make one",
		`  - namespace "default", Secret "web", key "tls.key": the store has no value`,
		`  - namespace "default", Secret "web", key "tls.crt": the store has no value`,
	} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error has no %q:\n%s", part, err)
		}
	}
	if got := listing(t, caroot); got != "" {
		t.Errorf("CAROOT got files: %s", got)
	}
}

// The success path of the CA in CAROOT. A run without the switch makes the
// CA in CAROOT and the leaf in the store. Then a run with the switch and an
// empty store refuses the two leaf entries only, not the CA. A run with the
// switch and the full store emits what the run without it emitted, and
// CAROOT and the store stay as they were.
func TestNoMint_UsesTheCAInCAROOT(t *testing.T) {
	caroot := t.TempDir()
	t.Setenv("CAROOT", caroot)
	spec := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: web, namespace: default}
type: kubernetes.io/tls
cert:
  hosts: [web.example.test]
`)
	dir := encStore(t)
	want, err := runEntry(t, spec, map[string]string{PathSecretsEnv: dir})
	if err != nil {
		t.Fatalf("the run without the switch: %v", err)
	}
	ca := listing(t, caroot)
	if ca == "" {
		t.Fatal("the run without the switch made no CA in CAROOT")
	}

	_, err = runEntry(t, spec, map[string]string{PathSecretsEnv: encStore(t), NoMintEnv: "1"})
	nerr, ok := errors.AsType[*NoMintError](err)
	if !ok {
		t.Fatalf("an empty store: err = %v, want a *NoMintError", err)
	}
	if len(nerr.Refused) != 2 || strings.Contains(err.Error(), "the CA in CAROOT") {
		t.Errorf("an empty store: want the two leaf entries only, got %+v:\n%s", nerr.Refused, err)
	}

	before := storeSnapshot(t, dir)
	got, err := runEntry(t, spec, map[string]string{PathSecretsEnv: dir, NoMintEnv: "1"})
	if err != nil {
		t.Fatalf("the full store: %v", err)
	}
	if got != want {
		t.Errorf("with the switch:\n%s\nwithout it:\n%s", got, want)
	}
	if after := listing(t, caroot); after != ca {
		t.Errorf("CAROOT changed:\nbefore %s\nafter  %s", ca, after)
	}
	if after := storeSnapshot(t, dir); !maps.Equal(after, before) {
		t.Errorf("the store changed")
	}
}

// An update: true bash: command still runs in a no-mint run: only its
// value is checked. The command leaves its marker in each case. A value
// that the store does not hold is refused as a write. The value that the
// store holds passes, and the run emits it. The store does not change.
func TestNoMint_AnUpdateCommandStillRuns(t *testing.T) {
	const line = `namespace "default", Secret "hook", key "OUT": the run would write a value that the store does not hold`
	for _, tc := range []struct {
		name, stored string
		refused      bool
	}{
		{"not in the store", "", true},
		{"another value in the store", "old", true},
		{"the same value in the store", "v", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "marker")
			spec := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: hook, namespace: default}
bash:
  OUT: {exec: "touch '` + marker + `'; printf v", update: true}
`)
			dir := encStore(t)
			if tc.stored != "" {
				if err := os.WriteFile(filepath.Join(dir, "Secret.hook.default.OUT"), []byte(tc.stored), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := storeSnapshot(t, dir)
			out, err := runEntry(t, spec, map[string]string{PathSecretsEnv: dir, NoMintEnv: "1"})
			if _, serr := os.Stat(marker); serr != nil {
				t.Errorf("the update: true command did not run: %v", serr)
			}
			if tc.refused {
				want := "LOK8S_SECRETS_NO_MINT is on, so the plugin writes nothing, but this run must write 1 value:\n  - " + line + noMintNext
				if err == nil || err.Error() != want {
					t.Errorf("error:\n%v\nwant:\n%s", err, want)
				}
				if out != "" {
					t.Errorf("emitted %q", out)
				}
			} else {
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if got := decodeSecretDataKey(t, out, "OUT"); got != "v" {
					t.Errorf("OUT = %q, want v", got)
				}
			}
			if after := storeSnapshot(t, dir); !maps.Equal(after, before) {
				t.Errorf("the store changed:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}
