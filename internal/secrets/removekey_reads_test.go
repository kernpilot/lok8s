package secrets

// removekey_reads_test.go: what remove-key reads before its first write, and
// how it writes each .enc. The recipients of an .enc come from its parsed
// sops metadata; a YAML form of .sops.yaml that the twins read differently is
// refused; a path it cannot stat is refused; an .enc that is not a managed
// Secret.*.enc is refused; an .enc is written through a temp file and a
// rename. tests/unit/secrets_remove_key_test.bats runs the same
// cases through the bash twin.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// jsonEscaped spells a recipient for a JSON string so that a JSON read
// decodes it to the recipient while the bytes of the file do not hold it: its
// first letter as the escape backslash, u, 0061.
func jsonEscaped(rcpt string) string {
	return string(rune(0x5c)) + "u0061" + rcpt[1:]
}

// The recipients of an .enc, read from its sops metadata. A row with no want
// is refused as metadata that does not parse or lists no age recipient.
func TestEncRecipients(t *testing.T) {
	foldedK := string(rune(0x212a)) // the Kelvin sign: it folds to k
	for _, tc := range []struct {
		name, raw string
		want      []string
	}{
		{"a flat age list", `{"sops":{"age":[{"recipient":"A"},{"recipient":"B"}]}}`, []string{keyA, keyB}},
		{"an escaped recipient", `{"sops":{"age":[{"recipient":"` + jsonEscaped(keyA) + `"}]}}`, []string{keyA}},
		{"key groups only", `{"sops":{"key_groups":[{"age":[{"recipient":"A"}]},{"age":[{"recipient":"B"}]}]}}`, []string{keyA, keyB}},
		{"a flat list and key groups", `{"sops":{"age":[{"recipient":"A"}],"key_groups":[{"age":[{"recipient":"B"}]}]}}`, []string{keyA, keyB}},
		{"not JSON", "enc\n", nil},
		{"a JSON list", `[{"sops":{"age":[{"recipient":"A"}]}}]`, nil},
		{"no sops object", `{"data":"x"}`, nil},
		{"a sops value that is not an object", `{"sops":"A"}`, nil},
		{"no age metadata", `{"sops":{"lastmodified":"x"}}`, nil},
		{"an empty age list", `{"sops":{"age":[]}}`, nil},
		{"an age value that is not a list", `{"sops":{"age":{"recipient":"A"}}}`, nil},
		{"an age entry with no recipient", `{"sops":{"age":[{"recipient":"A"},{"enc":"x"}]}}`, nil},
		{"a recipient that is not a string", `{"sops":{"age":[{"recipient":5}]}}`, nil},
		{"an empty recipient", `{"sops":{"age":[{"recipient":""}]}}`, nil},
		{"a key_groups value that is not a list", `{"sops":{"key_groups":{"age":[{"recipient":"A"}]}}}`, nil},
		{"a key group that is not an object", `{"sops":{"age":[{"recipient":"A"}],"key_groups":["B"]}}`, nil},
		{"age in another case", `{"sops":{"age":[{"recipient":"A"}],"AGE":[{"recipient":"B"}]}}`, nil},
		{"recipient in another case", `{"sops":{"age":[{"recipient":"A","Recipient":"B"}]}}`, nil},
		{"a key that folds to key_groups", `{"sops":{"age":[{"recipient":"A"}],"` + foldedK + `ey_groups":[{"age":[{"recipient":"B"}]}]}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.NewReplacer(`"A"`, `"`+keyA+`"`, `"B"`, `"`+keyB+`"`).Replace(tc.raw)
			got, err := encRecipients([]byte(raw))
			switch {
			case tc.want == nil && !errors.Is(err, errEncMetadata):
				t.Errorf("got %q (%v), want the metadata error", got, err)
			case tc.want != nil && (err != nil || !slices.Equal(got, tc.want)):
				t.Errorf("got %q (%v), want %q", got, err, tc.want)
			}
		})
	}
}

// A member twice in the sops metadata: a map decode keeps the last one, so the
// first could hide a recipient. Each row parses and lists an age recipient in
// its last members; the read refuses it as a duplicate. A row with a want
// holds no duplicate in the metadata. tests/unit/secrets_remove_key_test.bats
// reads the same rows through the bash twin.
func TestEncRecipientsRefusesADuplicateMember(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      []string
	}{
		{"the sops member twice", `{"data":"x","sops":{"key_groups":[{"age":[{"recipient":"B"},{"recipient":"A"}]}]},"sops":{"age":[{"recipient":"A"}]}}`, nil},
		{"the sops member twice, apart", `{"sops":{"age":[{"recipient":"B"}]},"data":"x","sops":{"age":[{"recipient":"A"}]}}`, nil},
		{"the sops member twice, the second spelled with an escape", `{"sops":{"age":[{"recipient":"B"}]},"` + jsonEscapedName("sops") + `":{"age":[{"recipient":"A"}]}}`, nil},
		{"age twice", `{"sops":{"age":[{"recipient":"B"},{"recipient":"A"}],"age":[{"recipient":"A"}]}}`, nil},
		{"key_groups twice", `{"sops":{"key_groups":[{"age":[{"recipient":"B"}]}],"key_groups":[{"age":[{"recipient":"A"}]}]}}`, nil},
		{"age twice in a key group", `{"sops":{"key_groups":[{"age":[{"recipient":"B"}],"age":[{"recipient":"A"}]}]}}`, nil},
		{"recipient twice in an entry", `{"sops":{"age":[{"recipient":"B","recipient":"A","enc":"x"}]}}`, nil},
		{"enc twice in an entry", `{"sops":{"age":[{"recipient":"A","enc":"x","enc":"y"}]}}`, nil},
		{"another member of sops twice", `{"sops":{"age":[{"recipient":"A"}],"mac":"x","mac":"y"}}`, nil},
		{"a member twice deeper in sops", `{"sops":{"age":[{"recipient":"A"}],"other":[{"x":{"y":1,"y":2}}]}}`, nil},
		{"a value and an object under one name", `{"sops":{"age":[{"recipient":"A"}],"mac":"x","mac":{"y":1}}}`, nil},
		{"an object and a value under one name", `{"sops":{"age":[{"recipient":"A"}],"mac":{"y":1},"mac":"x"}}`, nil},
		{"an empty object and an object under one name", `{"sops":{"age":[{"recipient":"A"}],"mac":{},"mac":{"y":1}}}`, nil},
		{"data twice", `{"data":"x","data":"y","sops":{"age":[{"recipient":"A"}]}}`, []string{keyA}},
		{"one name in two key groups", `{"sops":{"key_groups":[{"age":[{"recipient":"A"}]},{"age":[{"recipient":"B"}]}]}}`, []string{keyA, keyB}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.NewReplacer(`"A"`, `"`+keyA+`"`, `"B"`, `"`+keyB+`"`).Replace(tc.raw)
			got, err := encRecipients([]byte(raw))
			switch {
			case tc.want == nil && !errors.Is(err, errEncDuplicate):
				t.Errorf("got %q (%v), want the duplicate error", got, err)
			case tc.want != nil && (err != nil || !slices.Equal(got, tc.want)):
				t.Errorf("got %q (%v), want %q", got, err, tc.want)
			}
		})
	}
	// The shape is read first: a file with a duplicate whose last member does
	// not parse is refused as metadata that does not parse, in both twins.
	if _, err := encRecipients([]byte(`{"sops":{"age":[{"recipient":"` + keyA + `"}]},"sops":"x"}`)); !errors.Is(err, errEncMetadata) {
		t.Errorf("a duplicate sops member with a last value that is not an object: got %v, want the metadata error", err)
	}
}

// jsonEscapedName spells a member name with its first letter as a JSON
// escape: a JSON read decodes it to the name.
func jsonEscapedName(name string) string {
	return fmt.Sprintf("%cu%04x%s", 0x5c, name[0], name[1:])
}

// orderedObject writes a JSON object from its members in the given order, a
// name twice included: the form that encoding/json does not write.
func orderedObject(t *testing.T, members ...[2]string) string {
	t.Helper()
	parts := make([]string, 0, len(members))
	for _, m := range members {
		name, err := json.Marshal(m[0])
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, string(name)+":"+m[1])
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// lastWinsRecipients is the read of the recipients without the duplicate
// check: a map decode (as encRecipients decodes), which keeps the last of two
// members. A struct decode would not do: encoding/json decodes a second
// object into the same struct and merges the two.
func lastWinsRecipients(t *testing.T, raw string) []string {
	t.Helper()
	var top map[string]any
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatal(err)
	}
	var out []string
	add := func(list any) {
		items, _ := list.([]any)
		for _, item := range items {
			entry, _ := item.(map[string]any)
			if r, ok := entry["recipient"].(string); ok {
				out = append(out, r)
			}
		}
	}
	meta, _ := top["sops"].(map[string]any)
	add(meta["age"])
	groups, _ := meta["key_groups"].([]any)
	for _, group := range groups {
		if m, ok := group.(map[string]any); ok {
			add(m["age"])
		}
	}
	return out
}

// The files of a duplicate sops member, made from an .enc that the sops
// library wrote for the recipients k and g (g the one to remove). In each
// one a read that keeps the last member does not list g. The data payload,
// the data keys and the other metadata are those of the library file.
func duplicateFixtures(t *testing.T, raw, k, g string) map[string]string {
	t.Helper()
	var top, meta map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(top["sops"], &meta); err != nil {
		t.Fatal(err)
	}
	var entries []map[string]string
	if err := json.Unmarshal(meta["age"], &entries); err != nil {
		t.Fatal(err)
	}
	var kEntry, gEntry string
	gEnc := ""
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		switch e["recipient"] {
		case k:
			kEntry = string(b)
		case g:
			gEntry, gEnc = string(b), e["enc"]
		}
	}
	if kEntry == "" || gEntry == "" {
		t.Fatal("fixture: the library file does not list both recipients")
	}
	encValue, err := json.Marshal(gEnc)
	if err != nil {
		t.Fatal(err)
	}
	// The other members of the sops object, in a fixed order.
	var rest [][2]string
	for _, name := range slices.Sorted(maps.Keys(meta)) {
		if name != "age" {
			rest = append(rest, [2]string{name, string(meta[name])})
		}
	}
	sops := func(first ...[2]string) string { return orderedObject(t, append(first, rest...)...) }
	data := [2]string{"data", string(top["data"])}
	both, kept := "["+gEntry+","+kEntry+"]", "["+kEntry+"]"
	groups := func(list string) string { return `[{"age":` + list + `}]` }
	quoted := func(s string) string { return `"` + s + `"` }
	return map[string]string{
		"recipient twice in the entry of g": orderedObject(t, data, [2]string{"sops", sops([2]string{"age",
			"[" + orderedObject(t, [2]string{"recipient", quoted(g)}, [2]string{"recipient", quoted(k)}, [2]string{"enc", string(encValue)}) + "," + kEntry + "]"})}),
		"the sops member twice": orderedObject(t, data, [2]string{"sops", sops([2]string{"key_groups", groups(both)})},
			[2]string{"sops", orderedObject(t, [2]string{"age", kept})}),
		"age twice":        orderedObject(t, data, [2]string{"sops", sops([2]string{"age", both}, [2]string{"age", kept})}),
		"key_groups twice": orderedObject(t, data, [2]string{"sops", sops([2]string{"key_groups", groups(both)}, [2]string{"key_groups", groups(kept)})}),
	}
}

// A duplicate member through the command, on files made from a real .enc: the
// refusal names the file and says to encrypt it again; nothing is written. The
// first case is a file that the sops library still opens with the identity to
// remove, while a read that keeps the last member does not list it. The
// second, a second sops member, is a file that the sops library refuses:
// remove-key names the duplicate, not a metadata that does not parse.
func TestRemoveKeyRefusesADuplicateMember(t *testing.T) {
	for _, name := range []string{"recipient twice in the entry of g", "the sops member twice", "age twice", "key_groups twice"} {
		t.Run(name, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			c.Domain = "a.dev"
			keep, gone := newAgeIdentity(t), newAgeIdentity(t)
			k, g := keep.Recipient().String(), gone.Recipient().String()
			sopsRules(t, c, k+","+g)
			a := c.Paths.Clusters + "/a.dev/secrets"
			enc := a + "/Secret.app.default.K.enc"
			write(t, a+"/Secret.app.default.K", "v")
			encryptStore(t, c, "a.dev", out, errOut)
			fixture := duplicateFixtures(t, readFile(t, enc), k, g)[name]
			write(t, enc, fixture)
			freshen(t, a+"/Secret.app.default.K")
			if slices.Contains(lastWinsRecipients(t, fixture), g) {
				t.Fatal("fixture: a read that keeps the last member lists the recipient to remove")
			}
			switch name {
			case "recipient twice in the entry of g":
				if !opens(t, enc, gone, "v") {
					t.Fatal("fixture: the sops library does not open the file with the identity to remove")
				}
			case "the sops member twice":
				if _, err := binaryStore().LoadEncryptedFile([]byte(fixture)); err == nil {
					t.Fatal("fixture: the sops library loads a file with the sops member twice")
				}
				if opens(t, enc, gone, "v") || opens(t, enc, keep, "v") {
					t.Fatal("fixture: the sops library opens a file with the sops member twice")
				}
			}
			before := readFile(t, c.sopsConfigPath())

			if err := c.RemoveKey(g, false, false); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
			}
			want := "[error] the sops metadata of clusters/a.dev/secrets/Secret.app.default.K.enc holds a duplicate member\n" +
				"[error]   a duplicate member can hide a recipient from this check — re-encrypt those files with sops from their plaintext, then run this command again; nothing was written\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			unchanged(t, c, before)
			if readFile(t, enc) != fixture {
				t.Error("the .enc changed before the refusal")
			}
		})
	}
}

// hookWriter is an io.Writer that runs hook once, before the first write
// whose bytes hold mark.
type hookWriter struct {
	buf  bytes.Buffer
	mark string
	hook func()
}

func (w *hookWriter) Write(p []byte) (int, error) {
	if w.hook != nil && bytes.Contains(p, []byte(w.mark)) {
		hook := w.hook
		w.hook = nil
		hook()
	}
	return w.buf.Write(p)
}

// ageTwice is raw, an .enc that the sops library wrote, with the age list of
// its sops metadata twice: the first list also holds an entry for g, the last
// one is the list of the file. A read that keeps the last member does not
// list g.
func ageTwice(t *testing.T, raw, g string) string {
	t.Helper()
	var top, meta map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(top["sops"], &meta); err != nil {
		t.Fatal(err)
	}
	list := strings.TrimSpace(string(meta["age"]))
	if !strings.HasPrefix(list, "[") {
		t.Fatal("fixture: the age metadata of the library file is not a list")
	}
	members := [][2]string{{"age", `[{"recipient":"` + g + `","enc":"x"},` + list[1:]}, {"age", list}}
	for _, name := range slices.Sorted(maps.Keys(meta)) {
		if name != "age" {
			members = append(members, [2]string{name, string(meta[name])})
		}
	}
	return orderedObject(t, [2]string{"data", string(top["data"])}, [2]string{"sops", orderedObject(t, members...)})
}

// The post-check reads each .enc again after the re-key and refuses a
// duplicate member there too: a re-encrypt that writes metadata with the age
// list twice (the first one with the removed recipient) fails the run, which
// names the file and prints the command that resumes it. The encrypt runs in
// process and has no seam, so the test writes the duplicate into the
// re-keyed .enc through c.Out: rekeyFiles prints the count line of a store
// after its last encrypt and before the post-check reads the files.
// tests/unit/secrets_remove_key_test.bats runs the same case with a sops
// stub.
func TestRemoveKeyPostCheckRefusesADuplicateMember(t *testing.T) {
	c, out, errOut := testEnv(t)
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	_, enc, _ := otherEncStore(t, c, k, g, out, errOut)
	fixture := ""
	hooked := &hookWriter{mark: "Encrypted 1 secret(s)\n", hook: func() {
		if !opens(t, enc, keep, "v") || opens(t, enc, gone, "v") {
			t.Fatal("fixture: the .enc is not re-keyed when the count line is printed")
		}
		fixture = ageTwice(t, readFile(t, enc), g)
		write(t, enc, fixture)
	}}
	c.Out = hooked

	callerIdentity(t, keep)
	if err := c.RemoveKey(g, false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
	}
	if fixture == "" {
		t.Fatalf("the hook did not run: no count line on stdout:\n%s", hooked.buf.String())
	}
	if slices.Contains(lastWinsRecipients(t, fixture), g) {
		t.Fatal("fixture: a read that keeps the last member lists the removed recipient")
	}
	want := scopeWarn +
		"[error] the sops metadata of clusters/a.dev/secrets/Secret.app.default.K.enc holds a duplicate member\n" +
		"[error] 0 .enc file(s) still list the removed recipient after the re-key, 1 cannot be read or do not parse\n" +
		"[error] the re-key did not finish. .sops.yaml no longer lists the recipient, and running the same command again, with an identity that decrypts every file, resumes it:\n" +
		"[error]   lo secrets remove-key " + g + " --domain a.dev\n"
	if errOut.String() != want {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), want)
	}
	if readFile(t, enc) != fixture {
		t.Error("the .enc is not the file with the duplicate member")
	}
	if strings.Contains(readFile(t, c.sopsConfigPath()), g) {
		t.Error(".sops.yaml still lists the removed recipient: the run stopped before the first write")
	}
}

// The post-check repeats the reads and the walk of the check before the first
// write, so a file that appears in the store during the run fails it, by its
// path, with the command that resumes the run: another .enc, a managed .enc
// with a decrypted twin that the run did not re-key, an orphan, and a
// directory that it cannot read. With --skip-orphans an orphan that lists the
// recipient is a warning. Each file appears through c.Out, after the re-key
// of the store and before the post-check (see
// TestRemoveKeyPostCheckRefusesADuplicateMember), as a copy of the .enc of
// the store before the run, which lists the recipient.
// tests/unit/secrets_remove_key_test.bats runs the same cases with a sops
// stub.
func TestRemoveKeyPostCheckRefusesAFileThatAppears(t *testing.T) {
	const changedLine = "[error] 1 file(s) of the stores changed during the run: the stores must be quiet while remove-key runs\n"
	for _, tc := range []struct {
		name        string
		skipOrphans bool
		perms       bool
		appear      func(t *testing.T, store, managed string)
		want        string // stderr between the scope line and the resume lines
	}{
		{
			name:   "another .enc",
			appear: func(t *testing.T, store, managed string) { write(t, store+"/backup.enc", managed) },
			want:   "[error] remove-key re-keys only the managed Secret.*.enc files; clusters/a.dev/secrets/backup.enc is another .enc file: re-key or remove it by hand, then run again\n" + changedLine,
		},
		{
			name: "a managed .enc with a decrypted twin",
			appear: func(t *testing.T, store, managed string) {
				write(t, store+"/Secret.new.default.K", "v")
				write(t, store+"/Secret.new.default.K.enc", managed)
			},
			want: "[error] a managed .enc that this run did not re-key: clusters/a.dev/secrets/Secret.new.default.K.enc\n" + changedLine,
		},
		{
			name:   "an orphan",
			appear: func(t *testing.T, store, managed string) { write(t, store+"/Secret.orphan.default.K.enc", managed) },
			want:   "[error] an .enc with no decrypted twin after the re-key: clusters/a.dev/secrets/Secret.orphan.default.K.enc\n" + changedLine,
		},
		{
			name:        "an orphan with --skip-orphans",
			skipOrphans: true,
			appear:      func(t *testing.T, store, managed string) { write(t, store+"/Secret.orphan.default.K.enc", managed) },
		},
		{
			name:  "a directory that it cannot read",
			perms: true,
			appear: func(t *testing.T, store, _ string) {
				sub := mkdir(t, store+"/sub")
				if err := os.Chmod(sub, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
			},
			want: "[error] cannot read clusters/a.dev/secrets/sub\n" +
				"[error] 0 .enc file(s) still list the removed recipient after the re-key, 1 cannot be read or do not parse\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.perms {
				skipUnlessPermissionsHold(t)
			}
			c, out, errOut := testEnv(t)
			keep, gone := newAgeIdentity(t), newAgeIdentity(t)
			k, g := keep.Recipient().String(), gone.Recipient().String()
			store, enc, managed := otherEncStore(t, c, k, g, out, errOut)
			ran := false
			hooked := &hookWriter{mark: "Encrypted 1 secret(s)\n", hook: func() {
				ran = true
				tc.appear(t, store, managed)
			}}
			c.Out = hooked

			callerIdentity(t, keep)
			err := c.RemoveKey(g, false, tc.skipOrphans)
			if !ran {
				t.Fatalf("the hook did not run: no count line on stdout:\n%s", hooked.buf.String())
			}
			if tc.skipOrphans {
				if err != nil {
					t.Fatalf("%v\n%s", err, errOut.String())
				}
				want := scopeWarn + "[warn] 1 orphan .enc file(s) still list the removed recipient — its key can still decrypt them\n" + rotateWarn
				if errOut.String() != want {
					t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), want)
				}
			} else {
				if !errors.Is(err, ErrHandled) {
					t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
				}
				want := scopeWarn + tc.want +
					"[error] the re-key did not finish. .sops.yaml no longer lists the recipient, and running the same command again, with an identity that decrypts every file, resumes it:\n" +
					"[error]   lo secrets remove-key " + g + " --domain a.dev\n"
				if errOut.String() != want {
					t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), want)
				}
			}
			if opens(t, enc, gone, "v") || !opens(t, enc, keep, "v") {
				t.Error("the managed .enc was not re-keyed")
			}
		})
	}
}

// escapeIn rewrites the recipient in an .enc as its JSON escape: the file
// still opens to the recipient's identity, and its bytes no longer hold it.
func escapeIn(t *testing.T, enc, rcpt string) {
	t.Helper()
	raw := readFile(t, enc)
	if n := strings.Count(raw, rcpt); n != 1 {
		t.Fatalf("fixture: the recipient is in the .enc %d times, want 1", n)
	}
	write(t, enc, strings.Replace(raw, rcpt, jsonEscaped(rcpt), 1))
}

// The select scan reads the parsed metadata: an .enc that lists the
// recipient only as a JSON escape is re-keyed.
func TestRemoveKeySelectsAnEscapedRecipient(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	sopsRules(t, c, k+","+g)
	a := c.Paths.Clusters + "/a.dev/secrets"
	write(t, a+"/Secret.app.default.K", "v")
	encryptStore(t, c, "a.dev", out, errOut)
	escapeIn(t, a+"/Secret.app.default.K.enc", g)
	freshen(t, a+"/Secret.app.default.K")
	if !opens(t, a+"/Secret.app.default.K.enc", gone, "v") {
		t.Fatal("fixture: the escaped .enc does not open to the identity to remove")
	}

	callerIdentity(t, keep)
	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "re-keying 1 cache file(s) across 1 store(s)…\n") {
		t.Errorf("stdout:\n%q", out.String())
	}
	if opens(t, a+"/Secret.app.default.K.enc", gone, "v") || !opens(t, a+"/Secret.app.default.K.enc", keep, "v") {
		t.Error("the .enc with the escaped recipient was not re-keyed")
	}
}

// The post-check reads the parsed metadata too: an orphan that lists the
// recipient only as a JSON escape is named in the warning of --skip-orphans.
func TestRemoveKeyPostCheckReadsAnEscapedRecipient(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	sopsRules(t, c, k+","+g)
	a := c.Paths.Clusters + "/a.dev/secrets"
	write(t, a+"/Secret.app.default.K", "v")
	encryptStore(t, c, "a.dev", out, errOut)
	freshen(t, a+"/Secret.app.default.K")
	write(t, a+"/Secret.orphan.default.K.enc", encEnvelope(k, jsonEscaped(g)))

	callerIdentity(t, keep)
	if err := c.RemoveKey(g, false, true); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	wantErr := "[warn] orphan (no decrypted twin, cannot re-key): clusters/a.dev/secrets/Secret.orphan.default.K.enc\n" +
		scopeWarn +
		"[warn] 1 orphan .enc file(s) still list the removed recipient — its key can still decrypt them\n" +
		rotateWarn
	if errOut.String() != wantErr {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), wantErr)
	}
}

// An .enc whose sops metadata does not parse, or lists no age recipient, is
// refused before any write: remove-key cannot tell whether it lists the
// recipient. The refusal names the file only.
func TestRemoveKeyRefusesAnEncItCannotParse(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"not JSON", "enc\n"},
		{"no age metadata", `{"data":"ENC[x]","sops":{"lastmodified":"x"}}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			c.Domain = "a.dev"
			keep, gone := newAgeIdentity(t), newAgeIdentity(t)
			k, g := keep.Recipient().String(), gone.Recipient().String()
			sopsRules(t, c, k+","+g)
			a := c.Paths.Clusters + "/a.dev/secrets"
			write(t, a+"/Secret.app.default.K", "v")
			encryptStore(t, c, "a.dev", out, errOut)
			write(t, a+"/Secret.bad.default.K.enc", tc.content)
			write(t, a+"/Secret.bad.default.K", "b")
			freshen(t, a+"/Secret.app.default.K", a+"/Secret.bad.default.K")
			before := readFile(t, c.sopsConfigPath())

			if err := c.RemoveKey(g, false, false); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
			}
			want := "[error] the sops metadata of clusters/a.dev/secrets/Secret.bad.default.K.enc does not parse or lists no age recipient\n" +
				"[error]   remove-key reads the recipients of each .enc from its sops metadata — fix or restore those files first; nothing was written\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			unchanged(t, c, before)
			if !opens(t, a+"/Secret.app.default.K.enc", gone, "v") {
				t.Error("an .enc was re-keyed before the refusal")
			}
		})
	}
}

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

// A path the scans cannot stat may still hold an .enc that lists the
// recipient: a dangling .enc symlink (Lstat works, Stat does not), and with
// --all a domain directory it cannot search. Each is refused by its path
// before any write.
func TestRemoveKeyRefusesAPathItCannotStat(t *testing.T) {
	skipUnlessPermissionsHold(t)
	for _, tc := range []struct {
		name, want string
		all        bool
	}{
		{"a dangling .enc symlink", "clusters/a.dev/secrets/Secret.gone.default.K.enc", false},
		{"a domain directory with mode 000", "clusters/b.dev/secrets", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			c.Domain = "a.dev"
			keep, gone := newAgeIdentity(t), newAgeIdentity(t)
			k, g := keep.Recipient().String(), gone.Recipient().String()
			sopsRules(t, c, k+","+g)
			a := c.Paths.Clusters + "/a.dev/secrets"
			write(t, a+"/Secret.app.default.K", "v")
			encryptStore(t, c, "a.dev", out, errOut)
			freshen(t, a+"/Secret.app.default.K")
			if tc.all {
				b := mkdir(t, c.Paths.Clusters+"/b.dev/secrets")
				write(t, b+"/Secret.app.default.K.enc", encEnvelope(k, g))
				if err := os.Chmod(c.Paths.Clusters+"/b.dev", 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(c.Paths.Clusters+"/b.dev", 0o700) })
			} else if err := os.Symlink("Secret.nowhere.default.K.enc", a+"/Secret.gone.default.K.enc"); err != nil {
				t.Fatal(err)
			}
			before := readFile(t, c.sopsConfigPath())

			if err := c.RemoveKey(g, tc.all, false); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
			}
			want := "[error] cannot read " + tc.want + "\n" +
				"[error]   a file that is not read may still list the recipient — fix the permissions first; nothing was written\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			unchanged(t, c, before)
			if !opens(t, a+"/Secret.app.default.K.enc", gone, "v") {
				t.Error("an .enc was re-keyed before the refusal")
			}
		})
	}
}

// With --all and no domain store it can read (each domain directory mode
// 000), the refusal names each store path, in the form of the other
// unreadable refusals; it does not say that no store was found.
func TestRemoveKeyAllNamesEveryStoreItCannotRead(t *testing.T) {
	skipUnlessPermissionsHold(t)
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	k, g := newAgeIdentity(t).Recipient().String(), newAgeIdentity(t).Recipient().String()
	sopsRules(t, c, k+","+g)
	for _, d := range []string{"a.dev", "b.dev"} {
		dir := c.Paths.Clusters + "/" + d
		write(t, mkdir(t, dir+"/secrets")+"/Secret.app.default.K.enc", encEnvelope(k, g))
		if err := os.Chmod(dir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	}
	before := readFile(t, c.sopsConfigPath())

	if err := c.RemoveKey(g, true, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
	}
	want := "[error] cannot read clusters/a.dev/secrets\n" +
		"[error] cannot read clusters/b.dev/secrets\n" +
		"[error]   a file that is not read may still list the recipient — fix the permissions first; nothing was written\n"
	if errOut.String() != want || out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
	}
	unchanged(t, c, before)
}

// otherEncStore is a store of a.dev with one managed .enc that the sops
// library wrote for the recipients k and g (g the one to remove), in the
// state after a decrypt. It returns the store, the .enc and its bytes.
func otherEncStore(t *testing.T, c *Context, k, g string, out, errOut *bytes.Buffer) (store, enc, managed string) {
	t.Helper()
	c.Domain = "a.dev"
	sopsRules(t, c, k+","+g)
	store = c.Paths.Clusters + "/a.dev/secrets"
	write(t, store+"/Secret.app.default.K", "v")
	encryptStore(t, c, "a.dev", out, errOut)
	freshen(t, store+"/Secret.app.default.K")
	enc = store + "/Secret.app.default.K.enc"
	return store, enc, readFile(t, enc)
}

// An .enc file that is not a managed Secret.*.enc at the top of the store:
// remove-key re-keys only the managed files, so a copy of a valid ciphertext
// elsewhere in the store would keep the removed recipient. Each one is
// refused by its path before any write: a copy beside the managed files, a
// dot name, a file in a subdirectory (with a managed name), and a copy in a
// store that is a symlink to a directory. .sops.yaml and every .enc stay
// byte-equal. tests/unit/secrets_remove_key_test.bats runs the same cases.
func TestRemoveKeyRefusesAnotherEnc(t *testing.T) {
	for _, tc := range []struct {
		name, rel string
		linked    bool
	}{
		{"a copy beside the managed files", "backup.enc", false},
		{"a dot name", ".x.enc", false},
		{"a file in a subdirectory", "old/Secret.app.default.K.enc", false},
		{"a copy in a store that is a symlink to a directory", "backup.enc", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			keep, gone := newAgeIdentity(t), newAgeIdentity(t)
			k, g := keep.Recipient().String(), gone.Recipient().String()
			if tc.linked {
				target := mkdir(t, c.Paths.Base+"/real-store")
				mkdir(t, c.Paths.Clusters+"/a.dev")
				if err := os.Symlink(target, c.Paths.Clusters+"/a.dev/secrets"); err != nil {
					t.Fatal(err)
				}
			}
			store, enc, managed := otherEncStore(t, c, k, g, out, errOut)
			other := store + "/" + tc.rel
			write(t, other, managed)
			if !opens(t, other, gone, "v") {
				t.Fatal("fixture: the copy does not open to the identity to remove")
			}
			before := readFile(t, c.sopsConfigPath())

			if err := c.RemoveKey(g, false, false); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
			}
			want := "[error] remove-key re-keys only the managed Secret.*.enc files; clusters/a.dev/secrets/" + tc.rel + " is another .enc file: re-key or remove it by hand, then run again\n" +
				"[error]   an .enc file that is not re-keyed can keep the removed recipient — nothing was written\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			unchanged(t, c, before)
			if readFile(t, enc) != managed || readFile(t, other) != managed {
				t.Error("an .enc changed before the refusal")
			}
		})
	}
}

// A directory in the store that the walk cannot read may hold another .enc:
// it is refused by its path, in the form of the other unreadable refusals.
func TestRemoveKeyRefusesADirectoryItCannotRead(t *testing.T) {
	skipUnlessPermissionsHold(t)
	c, out, errOut := testEnv(t)
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	store, enc, managed := otherEncStore(t, c, k, g, out, errOut)
	sub := mkdir(t, store+"/sub")
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
	before := readFile(t, c.sopsConfigPath())

	if err := c.RemoveKey(g, false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
	}
	want := "[error] cannot read clusters/a.dev/secrets/sub\n" +
		"[error]   a file that is not read may still list the recipient — fix the permissions first; nothing was written\n"
	if errOut.String() != want || out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
	}
	unchanged(t, c, before)
	if readFile(t, enc) != managed {
		t.Error("the .enc changed before the refusal")
	}
}

// The other files of a store do not stop the walk: the .gitignore, the temp
// file that an encrypt of either twin left (.<name>.enc.tmp-<random>: it does
// not end in .enc), a restore.d working copy, a tls directory, and a
// directory named *.enc with no .enc in it.
func TestRemoveKeyWalksPastTheOtherFilesOfAStore(t *testing.T) {
	c, out, errOut := testEnv(t)
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	store, enc, _ := otherEncStore(t, c, k, g, out, errOut)
	for _, rel := range []string{
		".Secret.app.default.K.enc.tmp-123456789",
		".Secret.app.default.K.enc.tmp-AbC123",
		"restore.d/app.yaml",
		"tls/tls.crt",
		"dir.enc/notes",
	} {
		write(t, store+"/"+rel, "x")
	}
	if _, err := os.Stat(store + "/.gitignore"); err != nil {
		t.Fatalf("fixture: the store has no .gitignore: %v", err)
	}

	callerIdentity(t, keep)
	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if errOut.String() != scopeWarn+rotateWarn {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), scopeWarn+rotateWarn)
	}
	if opens(t, enc, gone, "v") || !opens(t, enc, keep, "v") {
		t.Error("the managed .enc was not re-keyed")
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

	callerIdentity(t, keep)
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
