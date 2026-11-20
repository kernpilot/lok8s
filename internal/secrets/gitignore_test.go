package secrets

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// The tests of the .gitignore plan (bash: tests/unit/secrets_test.bats).
// Each refusal line names the file, the line number and the line.

const testGitignoreRel = ".secrets/.gitignore"

// gitignoreBlockWith is the block with extra lines before its END line.
func gitignoreBlockWith(extra ...string) string {
	return strings.TrimSuffix(gitignoreContent, gitignoreEnd+"\n") + strings.Join(extra, "") + gitignoreEnd + "\n"
}

// refusal is the refusal of the line n of the .gitignore, then the final line.
func refusal(lines ...string) []string {
	return append(lines, gitignoreFinal(testGitignoreRel))
}

func lineRefusal(n, line, text string) string {
	return testGitignoreRel + ":" + n + ": " + line + ": " + text
}

func TestPlanGitignore(t *testing.T) {
	legacy := strings.Join(gitignoreLegacy, "\n") + "\n"
	legacyCRLF := strings.Join(gitignoreLegacy, "\r\n") + "\r\n"
	for _, tc := range []struct {
		name, raw, want string
		problems        []string
	}{
		{name: "an empty file gets the block", raw: "", want: gitignoreContent},
		// The old check found the allow line as a substring and wrote nothing.
		{name: "a file with comments only gets the block on top",
			raw:  "# *\n# !Secret.*.enc\n",
			want: gitignoreContent + "# *\n# !Secret.*.enc\n"},
		{name: "the rules of the user with no block get the block on top",
			raw:  "mine\n/tmp/\n",
			want: gitignoreContent + "mine\n/tmp/\n"},
		{name: "a last line with no newline stays as it is",
			raw:  "mine",
			want: gitignoreContent + "mine"},
		{name: "the legacy lines get the block in their place",
			raw:  legacy + "extra\n",
			want: gitignoreContent + "extra\n"},
		{name: "the legacy lines with CRLF get the block in their place",
			raw:  legacyCRLF + "extra\r\n",
			want: gitignoreContent + "extra\r\n"},
		{name: "a current block keeps every byte",
			raw:  "above\n" + gitignoreContent + "below\n!Secret.x.enc\n",
			want: "above\n" + gitignoreContent + "below\n!Secret.x.enc\n"},
		{name: "a changed block is rewritten, and the lines above and below stay byte for byte",
			raw: "above\r\n" + gitignoreBegin + "\r\n# an old comment\n*\n!.gitignore\n!Secret.*.enc\n" +
				gitignoreEnd + "\n" + "below\n\n# note\nlast",
			want: "above\r\n" + gitignoreContent + "below\n\n# note\nlast"},
		{name: "a comment and an empty line in the block are rewritten",
			raw:  gitignoreBlockWith("# mine\n", "\n"),
			want: gitignoreContent},
		{name: "a negation in the block is refused",
			raw:      gitignoreBlockWith("!Secret.app.default.K\n"),
			problems: refusal(lineRefusal("7", "!Secret.app.default.K", gitignoreNegationText))},
		{name: "another rule in the block is refused",
			raw:      gitignoreBlockWith("*.log\n"),
			problems: refusal(lineRefusal("7", "*.log", gitignoreInsideText))},
		{name: "a negation below the block is refused, with its line",
			raw:      "above\n" + gitignoreContent + "keep\n!*.yaml\n",
			problems: refusal(lineRefusal("10", "!*.yaml", gitignoreNegationText))},
		{name: "a negation in a file with no block is refused",
			raw:      "x\n!Secret.app.default.K\n",
			problems: refusal(lineRefusal("2", "!Secret.app.default.K", gitignoreNegationText))},
		{name: "a negation after the legacy lines is refused",
			raw:      legacy + "!*\n",
			problems: refusal(lineRefusal("6", "!*", gitignoreNegationText))},
		{name: "a negation above the block is dead, and it passes",
			raw:  "!plain\n" + gitignoreContent,
			want: "!plain\n" + gitignoreContent},
		// A pattern with a slash is read by its last part.
		{name: "a negation that re-includes only the .gitignore or the .enc files passes",
			raw: gitignoreContent + "!Secret.x.enc\n!/SECRET.*.ENC\n!restore.d/Secret.x.enc\n!**/Secret.*.enc\n" +
				"!Secret.*.enc   \n!.gitignore\n!secret.[ab].enc\n",
			want: gitignoreContent + "!Secret.x.enc\n!/SECRET.*.ENC\n!restore.d/Secret.x.enc\n!**/Secret.*.enc\n" +
				"!Secret.*.enc   \n!.gitignore\n!secret.[ab].enc\n"},
		{name: "a later positive rule passes",
			raw:  gitignoreContent + ".gitignore\nSecret.*.enc\n",
			want: gitignoreContent + ".gitignore\nSecret.*.enc\n"},
		{name: "each negation that can re-include a plaintext name is refused",
			raw: gitignoreContent + "!Secret.*\n!***/Secret.x\n!*\n!secret.enc\n!*.enc\n!x\\ \n" +
				"!Secret.*.enc\\\n!/**/plain\n!Secret*.enc\n!a/b\n!restore.d/*\n!tls/tls.crt\n!restore.d/\n!*/\n",
			problems: refusal(
				lineRefusal("8", "!Secret.*", gitignoreNegationText),
				lineRefusal("9", "!***/Secret.x", gitignoreNegationText),
				lineRefusal("10", "!*", gitignoreNegationText),
				lineRefusal("11", "!secret.enc", gitignoreNegationText),
				lineRefusal("12", "!*.enc", gitignoreNegationText),
				lineRefusal("13", "!x\\ ", gitignoreNegationText),
				lineRefusal("14", "!Secret.*.enc\\", gitignoreNegationText),
				lineRefusal("15", "!/**/plain", gitignoreNegationText),
				lineRefusal("16", "!Secret*.enc", gitignoreNegationText),
				// restore.d/ and tls/ of a store hold plaintext.
				lineRefusal("17", "!a/b", gitignoreNegationText),
				lineRefusal("18", "!restore.d/*", gitignoreNegationText),
				lineRefusal("19", "!tls/tls.crt", gitignoreNegationText),
				// Git reads a .gitignore in a directory that a negation
				// re-includes, and the block re-includes that file.
				lineRefusal("20", "!restore.d/", gitignoreNegationText),
				lineRefusal("21", "!*/", gitignoreNegationText))},
		{name: "a refused line shows no control byte and no CR",
			raw: gitignoreContent + "!\x1b[2Jx\n!plain\r\n",
			problems: refusal(
				lineRefusal("8", "!?[2Jx", gitignoreNegationText),
				lineRefusal("9", "!plain", gitignoreNegationText))},
		{name: "two BEGIN lines are refused",
			raw: gitignoreBegin + "\n" + gitignoreBegin + "\n" + gitignoreEnd + "\n",
			problems: refusal(
				lineRefusal("1", gitignoreBegin, gitignoreMarkerText),
				lineRefusal("2", gitignoreBegin, gitignoreMarkerText),
				lineRefusal("3", gitignoreEnd, gitignoreMarkerText))},
		{name: "an END line before the BEGIN line is refused",
			raw: gitignoreEnd + "\n" + gitignoreBegin + "\n",
			problems: refusal(
				lineRefusal("1", gitignoreEnd, gitignoreMarkerText),
				lineRefusal("2", gitignoreBegin, gitignoreMarkerText))},
		{name: "a NUL byte is refused",
			raw:      "a\x00b\n",
			problems: refusal(testGitignoreRel + ": the file has a NUL byte, and git ends a rule at that byte: remove it")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, problems := planGitignore([]byte(tc.raw), testGitignoreRel)
			if !slices.Equal(problems, tc.problems) {
				t.Errorf("problems:\n%q\nwant:\n%q", problems, tc.problems)
			}
			if tc.problems != nil {
				if want != nil {
					t.Errorf("a refused file has a plan: %q", want)
				}
				return
			}
			if string(want) != tc.want {
				t.Errorf("plan:\n%q\nwant:\n%q", want, tc.want)
			}
		})
	}
}

// ensureGitignore writes the plan, keeps each line of the user, and writes
// nothing when the file has the bytes of the plan. A file with comments only
// is not current: the old check read the allow line in a comment.
func TestEnsureGitignoreKeepsTheLinesOfTheUser(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"comments only", "# *\n# !Secret.*.enc\n", gitignoreContent + "# *\n# !Secret.*.enc\n"},
		{"above and below",
			"above\n" + gitignoreBegin + "\n# old\n*\n!.gitignore\n!Secret.*.enc\n" + gitignoreEnd + "\nbelow\nlast",
			"above\n" + gitignoreContent + "below\nlast"},
		{"legacy", strings.Join(gitignoreLegacy, "\n") + "\nmine\n", gitignoreContent + "mine\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, errOut := testEnv(t)
			dir := c.Paths.Base + "/.secrets"
			write(t, dir+"/.gitignore", tc.raw)
			if err := c.ensureGitignore(dir); err != nil {
				t.Fatalf("%v\n%s", err, errOut.String())
			}
			if got := readFile(t, dir+"/.gitignore"); got != tc.want {
				t.Errorf(".gitignore:\n%q\nwant:\n%q", got, tc.want)
			}
			before := fileStates(t, dir)
			if err := c.ensureGitignore(dir); err != nil {
				t.Fatalf("the second run: %v\n%s", err, errOut.String())
			}
			sameStates(t, before, fileStates(t, dir))
		})
	}
}

// A refused .gitignore: the refusal lines, ErrHandled, and nothing written.
func refusedGitignore(t *testing.T, c *Context, errOut *bytes.Buffer, err error, before map[string]string) {
	t.Helper()
	want := "[error] " + lineRefusal("8", "!Secret.app.default.K", gitignoreNegationText) + "\n" +
		"[error] " + gitignoreFinal(testGitignoreRel) + "\n"
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
	}
	if errOut.String() != want {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), want)
	}
	sameStates(t, before, fileStates(t, c.Paths.Base))
}

const reIncluding = "!Secret.app.default.K\n"

// A rerun of init with a config writes no config, but it checks the
// .gitignore of the store and writes it when it needs a change, in both
// branches: the key in the config, and another key (bash:
// tests/unit/secrets_test.bats).
func TestInitRerunChecksTheGitignore(t *testing.T) {
	for _, listed := range []bool{true, false} {
		name := "the key in the config"
		if !listed {
			name = "another key in the config"
		}
		t.Run(name, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			_, pubPath := testSSHKeypair(t, c.Paths.Base)
			agePubkey, _, err := deriveAgePublicKey(pubPath)
			if err != nil {
				t.Fatal(err)
			}
			if !listed {
				agePubkey = keyA
			}
			sopsRules(t, c, agePubkey)
			gitignore := c.Paths.Base + "/.secrets/.gitignore"
			write(t, gitignore, strings.Join(gitignoreLegacy, "\n")+"\nmine\n")
			if err := c.Init(pubPath); err != nil {
				t.Fatalf("%v\n%s", err, errOut.String())
			}
			if got := readFile(t, gitignore); got != gitignoreContent+"mine\n" {
				t.Errorf(".gitignore: %q", got)
			}
			if listed && out.String() != "age key already configured in .sops.yaml\n" {
				t.Errorf("stdout %q", out.String())
			}

			write(t, gitignore, gitignoreContent+reIncluding)
			out.Reset()
			errOut.Reset()
			before := fileStates(t, c.Paths.Base)
			refusedGitignore(t, c, errOut, c.Init(pubPath), before)
			if out.String() != "" {
				t.Errorf("stdout %q", out.String())
			}
		})
	}
}

// set checks the .gitignore before it writes the plaintext: a store with no
// .gitignore gets it, and a refused one stops the run with no plaintext
// (bash: tests/unit/secrets_test.bats).
func TestSetChecksTheGitignoreBeforeThePlaintext(t *testing.T) {
	c, _, errOut := testEnv(t)
	if err := c.Set(context.Background(), "app", "default", "K", "v", false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	dir := c.Paths.Base + "/.secrets"
	if got := readFile(t, dir+"/.gitignore"); got != gitignoreContent {
		t.Errorf(".gitignore: %q", got)
	}
	if err := os.Remove(dir + "/Secret.app.default.K"); err != nil {
		t.Fatal(err)
	}

	write(t, dir+"/.gitignore", gitignoreContent+reIncluding)
	errOut.Reset()
	before := fileStates(t, c.Paths.Base)
	refusedGitignore(t, c, errOut, c.Set(context.Background(), "app", "default", "K", "v", false), before)
}

// decrypt checks the .gitignore before the first plaintext, also before it
// looks for the age key (bash: tests/unit/secrets_test.bats).
func TestDecryptChecksTheGitignoreBeforeThePlaintext(t *testing.T) {
	c, _, errOut := testEnv(t)
	dir := c.Paths.Base + "/.secrets"
	write(t, dir+"/Secret.app.default.K.enc", encEnvelope(keyA))
	write(t, dir+"/.gitignore", gitignoreContent+reIncluding)
	before := fileStates(t, c.Paths.Base)
	refusedGitignore(t, c, errOut, c.Decrypt(c.Paths.Base+"/no-key"), before)

	write(t, dir+"/.gitignore", "mine\n")
	errOut.Reset()
	if err := c.Decrypt(c.Paths.Base + "/no-key"); !errors.Is(err, ErrHandled) {
		t.Fatalf("no age key: want ErrHandled, got %v\n%s", err, errOut.String())
	}
	if got := readFile(t, dir+"/.gitignore"); got != gitignoreContent+"mine\n" {
		t.Errorf(".gitignore: %q", got)
	}
}

// A rerun of add-key with nothing to do checks the .gitignore too, before it
// says "nothing to add" (bash: tests/unit/secrets_add_key_test.bats).
func TestAddKeyRerunRefusesAGitignoreThatReIncludesAPlaintext(t *testing.T) {
	f := newLinkFixture(t, "add-key")
	config := f.finished()
	write(t, f.store+"/.gitignore", gitignoreContent+"!*\n")
	before := fileStates(t, f.store)
	err := f.run()
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, f.errOut.String())
	}
	rel := "clusters/a.dev/secrets/.gitignore"
	want := "[error] " + rel + ":8: !*: " + gitignoreNegationText + "\n" + "[error] " + gitignoreFinal(rel) + "\n"
	if f.errOut.String() != want || f.out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
	}
	sameStates(t, before, fileStates(t, f.store))
	unchanged(t, f.c, config)
}
