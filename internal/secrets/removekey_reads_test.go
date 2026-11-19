package secrets

// removekey_reads_test.go: what remove-key reads before its first write, and
// how it writes each .enc. A YAML form of .sops.yaml that the twins read
// differently is refused; an .enc is written through a temp file and a
// rename. tests/unit/secrets_remove_key_test.bats runs the same cases
// through the bash twin.

import (
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The YAML forms remove-key refuses: an explicit tag under creation_rules
// other than !!str, !!seq or !!map, and a mapping key that is not a string.
// Implicit int, bool and null values, the three allowed tags and a merge key
// pass.
func TestYAMLFormCheck(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       yamlForm
	}{
		{"plain", "creation_rules:\n  - path_regex: x\n    age: 'A'\n", formOK},
		{"implicit int, bool and null values", "creation_rules:\n  - path_regex: x\n    shamir_threshold: 2\n    mac_only_encrypted: true\n    unencrypted_suffix: ~\n", formOK},
		{"the allowed tags", "creation_rules: !!seq\n  - !!map\n    path_regex: !!str x\n    age: !!str 'A'\n", formOK},
		{"a merge key", "base: &base\n  age: 'A'\ncreation_rules:\n  - <<: *base\n    path_regex: x\n", formOK},
		{"a tag outside creation_rules", "other: !!binary YWJj\ncreation_rules:\n  - age: 'A'\n", formOK},
		{"a quoted numeric key", "creation_rules:\n  - '1': y\n", formOK},
		{"a !!str key", "creation_rules:\n  - !!str 1: y\n", formOK},
		{"!!binary", "creation_rules:\n  - age: !!binary YWJj\n", formTag},
		{"a quoted !!binary", "creation_rules:\n  - age: !!binary \"YWJj\"\n", formTag},
		{"an explicit !!int", "creation_rules:\n  - shamir_threshold: !!int 2\n", formTag},
		{"a local tag", "creation_rules:\n  - age: !foo 'A'\n", formTag},
		{"a tag through an alias", "shared: &shared !!binary YWJj\ncreation_rules:\n  - age: *shared\n", formTag},
		{"a tag through a merge key", "base: &base\n  age: !!binary YWJj\ncreation_rules:\n  - <<: *base\n", formTag},
		{"creation_rules through a merge key", "base: &base\n  creation_rules:\n    - age: !!binary YWJj\n<<: *base\n", formTag},
		{"a numeric key", "creation_rules:\n  - path_regex: x\n    1: y\n", formKey},
		{"a bool key", "creation_rules:\n  - path_regex: x\n    true: y\n", formKey},
		{"a null key", "creation_rules:\n  - path_regex: x\n    ~: y\n", formKey},
		{"a numeric key outside creation_rules", "1: x\ncreation_rules: []\n", formKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(tc.yaml), &doc); err != nil {
				t.Fatal(err)
			}
			if got := yamlFormCheck(&doc); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// The same refusals through the command, before any write. In each case the
// bytes of the recipient are in the first rule only: the line edit would
// remove them there, and the second rule would keep the recipient in a form
// the other read does not see.
func TestRemoveKeyRefusesYAMLFormsItDoesNotRead(t *testing.T) {
	const tagged = "has a YAML tag under creation_rules other than !!str, !!seq or !!map — remove-key does not read tagged values; remove the tag by hand"
	const keyed = "has a mapping key that is not a string — remove-key does not read such a mapping; quote the key or remove it by hand"
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	first := "creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '" + k + "," + g + "'\n"
	shared := "shared: &shared " + escaped(g) + "\n"
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"a !!binary recipient", first + "  - path_regex: 'Other\\..*'\n    age: !!binary " + base64.StdEncoding.EncodeToString([]byte(g)) + "\n", tagged},
		{"a numeric key", shared + first + "  - path_regex: 'Other\\..*'\n    age: *shared\n    1: x\n", keyed},
		{"a bool key", shared + first + "  - path_regex: 'Other\\..*'\n    age: *shared\n    true: x\n", keyed},
		{"a null key", shared + first + "  - path_regex: 'Other\\..*'\n    age: *shared\n    ~: x\n", keyed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			c.Domain = "a.dev"
			write(t, c.sopsConfigPath(), first)
			a := c.Paths.Clusters + "/a.dev/secrets"
			write(t, a+"/Secret.app.default.K", "v")
			encryptStore(t, c, "a.dev", out, errOut)
			freshen(t, a+"/Secret.app.default.K")
			write(t, c.sopsConfigPath(), tc.yaml)
			if n := strings.Count(tc.yaml, g); n != 1 {
				t.Fatalf("fixture: the recipient must be in the bytes of the first rule only, found %d times", n)
			}

			if err := c.RemoveKey(g, false, false); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
			}
			want := "[error] " + c.sopsConfigPath() + " " + tc.want + "\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			unchanged(t, c, tc.yaml)
			if !opens(t, a+"/Secret.app.default.K.enc", gone, "v") {
				t.Error("an .enc was re-keyed before the refusal")
			}
		})
	}
}

// The write helper of every encrypt, called directly (in process, no seam):
// a write that stops half way leaves the old file whole and no temp file; a
// write that ends replaces the file (mode 0600) and leaves a hard link to the
// old one with the old content.
func TestWriteReplacingKeepsTheOldFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/Secret.app.default.K.enc"
	write(t, path, "old\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, dir+"/hard"); err != nil {
		t.Fatal(err)
	}
	entries := func() int {
		list, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}

	stop := errors.New("stopped half way")
	err := writeReplacing(path, func(w io.Writer) error {
		if _, err := io.WriteString(w, "ne"); err != nil {
			return err
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("want the write error, got %v", err)
	}
	if got := readFile(t, path); got != "old\n" {
		t.Errorf("after a failed write the file is %q, want the old content", got)
	}
	if n := entries(); n != 2 {
		t.Errorf("a failed write left %d entries, want 2 (no temp file)", n)
	}

	if err := writeReplacing(path, func(w io.Writer) error {
		_, err := io.WriteString(w, "new\n")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "new\n" {
		t.Errorf("file: %q", got)
	}
	if got := readFile(t, dir+"/hard"); got != "old\n" {
		t.Errorf("the hard link sees %q: the file was written in place", got)
	}
	if info, err := os.Stat(path); err != nil {
		t.Error(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
	if n := entries(); n != 2 {
		t.Errorf("%d entries, want 2 (no temp file)", n)
	}
}

// The same through remove-key and the real encrypt: the re-key replaces each
// .enc (a hard link keeps the old bytes), mode 0600, and leaves no temp file.
func TestRemoveKeyReplacesEachEnc(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	sopsRules(t, c, k+","+g)
	a := c.Paths.Clusters + "/a.dev/secrets"
	write(t, a+"/Secret.app.default.K", "v")
	encryptStore(t, c, "a.dev", out, errOut)
	freshen(t, a+"/Secret.app.default.K")
	enc := a + "/Secret.app.default.K.enc"
	old := readFile(t, enc)
	link := c.Paths.Base + "/old.enc"
	if err := os.Link(enc, link); err != nil {
		t.Fatal(err)
	}

	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if readFile(t, link) != old {
		t.Error("the .enc was written in place, not replaced")
	}
	if !opens(t, enc, keep, "v") {
		t.Error("the remaining identity cannot open the re-keyed .enc")
	}
	if info, err := os.Stat(enc); err != nil {
		t.Error(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
	list, err := os.ReadDir(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temp file was left: %s", e.Name())
		}
	}
}
