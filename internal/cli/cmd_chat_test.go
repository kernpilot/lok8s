package cli

// cmd_chat_test.go covers the `lo chat` exec argv and its preflight errors.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
)

func chatProject(t *testing.T) *config.Paths {
	t.Helper()
	return chatProjectAt(t, t.TempDir())
}

// chatProjectAt is chatProject with the project root at base.
func chatProjectAt(t *testing.T, base string) *config.Paths {
	t.Helper()
	p := synthProjectAt(t, base)
	testutil.WriteFile(t, filepath.Join(p.Bin, "lochat"), "#!/bin/sh\nexit 0\n")
	os.Chmod(filepath.Join(p.Bin, "lochat"), 0o755)
	testutil.WriteFile(t, filepath.Join(p.Bin, "argsh.so"), "")
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "chat", "defaults.json"), "{}\n")
	// The project holds the bash tree: --lo is its entrypoint (without one
	// it would be the cache copy the binary extracts).
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "lo"), "#!/usr/bin/env bash\n")
	return p
}

func TestChatExecArgv(t *testing.T) {
	p := chatProject(t)
	t.Setenv("LO_CHAT_CONFIG", "")
	os.Unsetenv("LO_CHAT_CONFIG")

	var gotBin string
	var gotArgv []string
	saved := execProcess
	execProcess = func(bin string, argv, env []string) error {
		gotBin, gotArgv = bin, argv
		return nil
	}
	defer func() { execProcess = saved }()

	if _, _, err := runLo(t, NewRoot(p), "chat", "-p", "hello", "--model", "m", "--help"); err != nil {
		t.Fatal(err)
	}
	if gotBin != filepath.Join(p.Bin, "lochat") {
		t.Errorf("bin = %q", gotBin)
	}
	want := []string{
		filepath.Join(p.Bin, "lochat"),
		"--config", filepath.Join(p.Lok8s, "chat", "defaults.json"),
		"--lo", filepath.Join(p.Lok8s, "lo"),
		"--cwd", p.Base,
		"--base-dir", p.Base,
		"-p", "hello", "--model", "m", "--help",
	}
	if strings.Join(gotArgv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %q\nwant  %q", gotArgv, want)
	}

	// A per-project lo-chat.json wins over the shipped defaults; LO_CHAT_CONFIG
	// overrides the project path.
	testutil.WriteFile(t, filepath.Join(p.Base, "lo-chat.json"), "{}\n")
	runLo(t, NewRoot(p), "chat")
	if gotArgv[2] != filepath.Join(p.Base, "lo-chat.json") {
		t.Errorf("project config not preferred: %q", gotArgv[2])
	}
	custom := filepath.Join(p.Base, "custom.json")
	testutil.WriteFile(t, custom, "{}\n")
	t.Setenv("LO_CHAT_CONFIG", custom)
	runLo(t, NewRoot(p), "chat")
	if gotArgv[2] != custom {
		t.Errorf("LO_CHAT_CONFIG not honoured: %q", gotArgv[2])
	}
}

func TestChatPreflightErrors(t *testing.T) {
	saved := execProcess
	execProcess = func(string, []string, []string) error { t.Fatal("exec reached"); return nil }
	defer func() { execProcess = saved }()
	t.Setenv("PATH", t.TempDir()) // no lochat/argsh on PATH

	p := synthProject(t)
	_, stderr, err := runLo(t, NewRoot(p), "chat")
	// The hint names the two places that were searched and points at the
	// install steps of the guide. A user project has no lochat source, so the
	// hint gives no build command.
	wantHint := lochatHint(p.Bin)
	if !errors.Is(err, ErrHandled) || stderr != wantHint {
		t.Errorf("missing lochat: err=%v\n got stderr=%q\nwant stderr=%q", err, stderr, wantHint)
	}

	testutil.WriteFile(t, filepath.Join(p.Bin, "lochat"), "#!/bin/sh\n")
	os.Chmod(filepath.Join(p.Bin, "lochat"), 0o755)
	// No project config and no local .lok8s/chat: the embedded defaults are
	// ejected on first use (the "no chat config" error is unreachable now),
	// and the preflight moves on to the argsh.so check.
	_, stderr, err = runLo(t, NewRoot(p), "chat")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "argsh.so is missing") || !strings.Contains(stderr, "  argsh builtins install") {
		t.Errorf("missing argsh.so: err=%v stderr=%q", err, stderr)
	}
	ejected, err := os.ReadFile(filepath.Join(p.Lok8s, "chat", "defaults.json"))
	if err != nil {
		t.Fatalf("chat defaults not ejected: %v", err)
	}
	embedded, _ := fs.ReadFile(assets.FS(), "chat/defaults.json")
	if !bytes.Equal(ejected, embedded) {
		t.Error("ejected defaults.json differs from the embedded copy")
	}
	if _, err := os.Stat(filepath.Join(p.Lok8s, "chat", assets.MarkerFile)); err != nil {
		t.Error("no .lo-origin marker next to the ejected defaults")
	}

	// LO_ASSETS_EJECT=never: a fresh project stays untouched. (chat passes
	// its argv through unparsed, so the env form is the one that reaches
	// it; the --no-eject flag is covered on a flag-parsing command in
	// cmd_assets_test.go.)
	t.Setenv(assets.EnvEject, "never")
	p2 := synthProject(t)
	testutil.WriteFile(t, filepath.Join(p2.Bin, "lochat"), "#!/bin/sh\n")
	os.Chmod(filepath.Join(p2.Bin, "lochat"), 0o755)
	_, _, _ = runLo(t, NewRoot(p2), "chat")
	if _, err := os.Stat(filepath.Join(p2.Lok8s, "chat")); err == nil {
		t.Error("LO_ASSETS_EJECT=never still wrote .lok8s/chat")
	}
}

// lochatHint is the hint for a missing lochat when PATH_BIN is bin.
func lochatHint(bin string) string {
	return "[error] lochat binary not found in " + bin + " or on PATH.\n" +
		"[error] To install it, do the set-up steps at https://lok8s.io/guide/lo-chat#set-up\n"
}

// chatExecBin swaps execProcess for the test and returns the binary that
// chat execs ("" until it execs).
func chatExecBin(t *testing.T) *string {
	t.Helper()
	var got string
	saved := execProcess
	execProcess = func(bin string, _, _ []string) error { got = bin; return nil }
	t.Cleanup(func() { execProcess = saved })
	t.Setenv("LO_CHAT_CONFIG", "")
	return &got
}

// lochatStub writes an executable lochat into dir and returns its path.
func lochatStub(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "lochat")
	testutil.WriteFile(t, bin, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// The PATH fallback: no lochat in PATH_BIN and a lochat on PATH. The check
// passes, the hint does not print, and chat runs the lochat from PATH (the
// bats twin: tests/unit/chat_test.bats).
func TestChatLochatOnPath(t *testing.T) {
	p := chatProject(t)
	if err := os.Remove(filepath.Join(p.Bin, "lochat")); err != nil {
		t.Fatal(err)
	}
	onPath := t.TempDir()
	want := lochatStub(t, onPath)
	t.Setenv("PATH", onPath)
	gotBin := chatExecBin(t)

	_, stderr, err := runLo(t, NewRoot(p), "chat")
	if err != nil || stderr != "" {
		t.Errorf("a lochat on PATH: err=%v stderr=%q, want no error and no output", err, stderr)
	}
	if *gotBin != want {
		t.Errorf("exec %q, want the lochat on PATH %q", *gotBin, want)
	}
}

// PATH_BIN that is not <base>/.bin: chat looks in paths.Bin (resolved from
// an exported PATH_BIN), never in a path rebuilt from the base. A decoy
// lochat in <base>/.bin tells the two apart, when the check passes and in
// the hint when it fails. A lochat on PATH too: the one in PATH_BIN wins.
func TestChatLochatInPathBin(t *testing.T) {
	synth := chatProject(t) // its lochat in <base>/.bin is the decoy
	custom := t.TempDir()
	t.Setenv("PATH_BASE", synth.Base)
	t.Setenv("PATH_BIN", custom)
	t.Setenv("PATH_LOK8S", "")
	t.Setenv("PATH_CLUSTERS", "")
	p, err := config.ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.Bin != custom || p.Lok8s != synth.Lok8s {
		t.Fatalf("paths: Bin=%q Lok8s=%q, want %q and %q", p.Bin, p.Lok8s, custom, synth.Lok8s)
	}
	want := lochatStub(t, custom)
	onPath := t.TempDir()
	lochatStub(t, onPath)
	t.Setenv("PATH", onPath)
	gotBin := chatExecBin(t)

	_, stderr, err := runLo(t, NewRoot(p), "chat")
	if err != nil || stderr != "" {
		t.Errorf("a lochat in PATH_BIN: err=%v stderr=%q, want no error and no output", err, stderr)
	}
	if *gotBin != want {
		t.Errorf("exec %q, want the lochat in PATH_BIN %q", *gotBin, want)
	}

	if err := os.Remove(want); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // no lochat on PATH
	*gotBin = ""
	_, stderr, err = runLo(t, NewRoot(p), "chat")
	wantHint := lochatHint(custom)
	if !errors.Is(err, ErrHandled) || stderr != wantHint || *gotBin != "" {
		t.Errorf("no lochat in PATH_BIN: err=%v exec=%q\n got stderr=%q\nwant stderr=%q", err, *gotBin, stderr, wantHint)
	}
}

// chatExportedPathBin is a chat project with PATH_BIN exported to another
// dir, which holds the lochat. It returns the paths that lo resolves from
// that environment and the exported dir. The project's .bin holds an
// argsh.so, the exported dir none. PATH is an empty dir, so a real argsh on
// the machine cannot decide the argsh.so check. The project root is root
// in a temp dir ("" for the temp dir itself).
func chatExportedPathBin(t *testing.T, root string) (*config.Paths, string) {
	t.Helper()
	synth := chatProjectAt(t, filepath.Join(t.TempDir(), root))
	exported := t.TempDir()
	t.Setenv("PATH_BASE", synth.Base)
	t.Setenv("PATH_BIN", exported)
	t.Setenv("PATH_LOK8S", "")
	t.Setenv("PATH_CLUSTERS", "")
	t.Setenv("PATH", t.TempDir())
	p, err := config.ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.Bin != exported || filepath.Join(p.Base, ".bin") != synth.Bin {
		t.Fatalf("paths: Bin=%q Base=%q, want %q and the parent of %q", p.Bin, p.Base, exported, synth.Bin)
	}
	lochatStub(t, exported)
	return p, exported
}

// The argsh.so check reads the PATH_BIN of the `lo` child, not an exported
// PATH_BIN: an argsh.so in the project's .bin passes, with none in the
// exported dir. The child gets the project's .bin as PATH_BIN, so the
// check and the child read the same dir.
func TestChatArgshSoCheckReadsTheChildPathBin(t *testing.T) {
	p, exported := chatExportedPathBin(t, "")
	var gotBin string
	var gotEnv []string
	saved := execProcess
	execProcess = func(bin string, _, env []string) error {
		gotBin, gotEnv = bin, env
		return nil
	}
	t.Cleanup(func() { execProcess = saved })
	t.Setenv("LO_CHAT_CONFIG", "")

	_, stderr, err := runLo(t, NewRoot(p), "chat")
	if err != nil || stderr != "" {
		t.Fatalf("argsh.so in the project's .bin: err=%v stderr=%q, want no error and no output", err, stderr)
	}
	if gotBin != filepath.Join(exported, "lochat") {
		t.Errorf("exec %q, want the lochat in PATH_BIN", gotBin)
	}
	var pathBin []string
	for _, kv := range gotEnv {
		if strings.HasPrefix(kv, "PATH_BIN=") {
			pathBin = append(pathBin, kv)
		}
	}
	if want := "PATH_BIN=" + filepath.Join(p.Base, ".bin"); len(pathBin) != 1 || pathBin[0] != want {
		t.Errorf("the PATH_BIN entries of the child env = %q, want exactly one: %s", pathBin, want)
	}
}

// The reverse: an argsh.so in the exported PATH_BIN only fails the check,
// because the child does not load it from there. The error names the
// project's .bin, never the exported dir, and the hint installs there
// (--force: argsh writes nothing when it loads an argsh.so from elsewhere).
// The hint quotes the dir for a POSIX shell: a single quote in it becomes
// a quote, a backslash and two quotes.
func TestChatArgshSoOnlyInAnExportedPathBinFailsTheCheck(t *testing.T) {
	for _, tc := range []struct {
		name, root string
		// quoted is the end of the quoted dir in the hint, after the parent
		// of the project root.
		quoted string
	}{
		{"plain", "proj", `/proj/.bin'`},
		{"single quote", "it's", `/it'\''s/.bin'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, exported := chatExportedPathBin(t, tc.root)
			testutil.WriteFile(t, filepath.Join(exported, "argsh.so"), "")
			bin := filepath.Join(p.Base, ".bin")
			if err := os.Remove(filepath.Join(bin, "argsh.so")); err != nil {
				t.Fatal(err)
			}
			gotBin := chatExecBin(t)

			_, stderr, err := runLo(t, NewRoot(p), "chat")
			want := "[error] argsh.so is missing from " + bin + ". 'lo chat' uses 'lo mcp', and 'lo mcp' is an argsh builtin in argsh.so.\n" +
				"[error] To install the latest release of argsh.so in that directory, run this command. Then try again:\n" +
				"[error]   argsh builtins install --force --path '" + filepath.Dir(p.Base) + tc.quoted + "\n"
			if !errors.Is(err, ErrHandled) || stderr != want || *gotBin != "" {
				t.Errorf("argsh.so in the exported PATH_BIN only: err=%v exec=%q\n got stderr=%q\nwant stderr=%q", err, *gotBin, stderr, want)
			}
		})
	}
}

// A project dir with a control character (a newline and an ESC): both
// errors print the dir Go-quoted, and the argsh.so error shows no command,
// so the name cannot end a line or drive the terminal.
func TestChatArgshSoErrorQuotesADirWithAControlCharacter(t *testing.T) {
	parent := t.TempDir()
	p := chatProjectAt(t, filepath.Join(parent, "a\nb\x1b[31m"))
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PATH_BIN", p.Bin)
	gotBin := chatExecBin(t)
	shown := `"` + parent + `/a\nb\x1b[31m/.bin"`

	if err := os.Remove(filepath.Join(p.Bin, "lochat")); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runLo(t, NewRoot(p), "chat")
	if want := lochatHint(shown); !errors.Is(err, ErrHandled) || stderr != want {
		t.Errorf("no lochat: err=%v\n got stderr=%q\nwant stderr=%q", err, stderr, want)
	}

	lochatStub(t, p.Bin)
	if err := os.Remove(filepath.Join(p.Bin, "argsh.so")); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = runLo(t, NewRoot(p), "chat")
	want := "[error] argsh.so is missing from " + shown + ". 'lo chat' uses 'lo mcp', and 'lo mcp' is an argsh builtin in argsh.so.\n" +
		"[error] The directory name has a control character, so lo does not show the command.\n" +
		"[error] Install the latest release of argsh.so in that directory. Then try again.\n"
	if !errors.Is(err, ErrHandled) || stderr != want || *gotBin != "" {
		t.Errorf("no argsh.so: err=%v exec=%q\n got stderr=%q\nwant stderr=%q", err, *gotBin, stderr, want)
	}
}

// An argsh on PATH with an argsh.so beside it does not pass the check: the
// child sources the project's .bin/argsh, and argsh does not load argsh.so
// from beside an argsh on PATH.
func TestChatArgshSoBesideAnArgshOnPathFailsTheCheck(t *testing.T) {
	p := chatProject(t)
	if err := os.Remove(filepath.Join(p.Bin, "argsh.so")); err != nil {
		t.Fatal(err)
	}
	onPath := t.TempDir()
	testutil.WriteFile(t, filepath.Join(onPath, "argsh"), "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(onPath, "argsh"), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(onPath, "argsh.so"), "")
	t.Setenv("PATH", onPath)
	t.Setenv("PATH_BIN", p.Bin)
	gotBin := chatExecBin(t)

	_, stderr, err := runLo(t, NewRoot(p), "chat")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "argsh.so is missing") || *gotBin != "" {
		t.Errorf("argsh.so beside an argsh on PATH only: err=%v exec=%q stderr=%q, want the argsh.so error", err, *gotBin, stderr)
	}
}

// argshToolName is the tool name the argsh `lo mcp` builtin gives a command:
// lo_ and the path, of which a command three or more levels deep keeps only
// its last two words (`lo kubehz space kubeconfig` is lo_space_kubeconfig).
// The builtin prefixes the parent's name onto its children only, never onto
// the grandchildren.
func argshToolName(c *cobra.Command) string {
	words := strings.Fields(c.CommandPath())[1:]
	if len(words) > 2 {
		words = words[len(words)-2:]
	}
	return "lo_" + strings.Join(words, "_")
}

func TestArgshToolName(t *testing.T) {
	root := newUsageTree(synthProject(t), routing{})
	for path, want := range map[string]string{
		"kubeconfig":                "lo_kubeconfig",
		"kubehz token":              "lo_kubehz_token",
		"kubehz node join":          "lo_node_join",
		"kubehz space kubeconfig":   "lo_space_kubeconfig",
		"kubehz cluster kubeconfig": "lo_cluster_kubeconfig",
	} {
		cmd := findByPath(root, path)
		if cmd == nil {
			t.Fatalf("no command %q", path)
		}
		if got := argshToolName(cmd); got != want {
			t.Errorf("%s: %s, want %s", path, got, want)
		}
	}
}

// TestChatTiersNameNoToolTheArgshServerCannotRun: the argsh `lo mcp` that
// `lo chat` drives names a command three levels deep by its last two words
// and then cannot run it (#245). A tier entry for such a tool would offer
// the model a tool that always fails. The deny list may name them.
func TestChatTiersNameNoToolTheArgshServerCannotRun(t *testing.T) {
	broken, runnable := map[string]bool{}, map[string]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if len(strings.Fields(c.CommandPath())) > 3 {
			broken[argshToolName(c)] = true
		} else {
			runnable[argshToolName(c)] = true
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newUsageTree(synthProject(t), routing{}))
	// A two-word name that a real two-level command also carries
	// (`lo drivers kubehz status` and `lo kubehz status`) runs that one.
	for name := range runnable {
		delete(broken, name)
	}
	if !broken["lo_space_list"] {
		t.Fatalf("no third-level tool found (%v): the check proves nothing", broken)
	}
	for _, file := range []string{"../../.lok8s/chat/defaults.json", "../assets/lok8s/chat/defaults.json"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var defaults struct {
			Injection struct {
				Tiers map[string][]string `json:"tiers"`
			} `json:"injection"`
		}
		if err := json.Unmarshal(raw, &defaults); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for tier, tools := range defaults.Injection.Tiers {
			for _, tool := range tools {
				if broken[tool] {
					t.Errorf("%s: tier %s names %s, which the argsh lo mcp cannot run (#245)", file, tier, tool)
				}
			}
		}
	}
}

// TestChatDeniesEveryCredentialOutputCommand: `lo chat` drives the bash tree's
// argsh `lo mcp`, which knows nothing of AnnotationCredentialOutput. The chat
// defaults' deny list is the only gate there, so every such command must be
// on it (in the project tree and in the embedded copy), under the name the
// argsh server gives it.
func TestChatDeniesEveryCredentialOutputCommand(t *testing.T) {
	var tools []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Annotations[AnnotationCredentialOutput] == "true" {
			tools = append(tools, argshToolName(c))
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newUsageTree(synthProject(t), routing{}))
	for _, must := range []string{"lo_kubehz_token", "lo_space_kubeconfig", "lo_cluster_kubeconfig"} {
		if !slices.Contains(tools, must) {
			t.Fatalf("credential-output commands = %v: %s lost its annotation, the check proves nothing", tools, must)
		}
	}
	for _, file := range []string{"../../.lok8s/chat/defaults.json", "../assets/lok8s/chat/defaults.json"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var defaults struct {
			Injection struct {
				Deny []string `json:"deny"`
			} `json:"injection"`
		}
		if err := json.Unmarshal(raw, &defaults); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, tool := range tools {
			if !slices.Contains(defaults.Injection.Deny, tool) {
				t.Errorf("%s: injection.deny lacks %s, a command whose output is a credential", file, tool)
			}
		}
		// And back: a denied tool that is not annotated would still be an
		// MCP tool of the Go server. This makes the chat deny list exactly
		// the credential-output set; a deny for another reason needs its
		// own list.
		for _, denied := range defaults.Injection.Deny {
			if !slices.Contains(tools, denied) {
				t.Errorf("%s: %s is denied to chat but not annotated %s", file, denied, AnnotationCredentialOutput)
			}
		}
	}
}
